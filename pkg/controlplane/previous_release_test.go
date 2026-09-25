package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/sshx"
)

// AFTER A FAILED MIGRATION THE PREVIOUS CHART MUST RUN BEHIND THE FENCE
// (kn-t70-control-plane-version-identity-4xso.2).
//
// The migration stage applies the chart with `backend.replicas: 0` and the NEW
// image (one image value for the Job and the Deployment), so a failed migration
// leaves the control plane running NOTHING: every read, through the fenced route
// or through the node's tunnel, is refused with "connection refused". The plan
// (7.8, S2(e)) asks for "the previous chart is what runs, the fence is still up,
// and the checkpoint is still eligible", so the previous release is captured
// before the first apply and put back when the migration fails.
const previousChartContent = "previous-candidate-chart-archive"

// previousKube is the smallest in-memory kube the previous release needs: the
// live HelmChart, the Secret that carries it, and the applies it observes.
type previousKube struct {
	mu sync.Mutex
	// chartContent and valuesContent are what the LIVE HelmChart holds.
	chartContent  string
	valuesContent string
	// secret is the fence's Secret, as the API server holds it: base64-decoded
	// values under data.
	secret map[string]string
	// applied records every chart apply, in order.
	applied []appliedRelease
	// chartRevision is the install revision the last apply stamped, so the
	// rollout probes can answer about it.
	chartRevision string
	// backendReady is how many replicas the fake reports READY.
	backendReady int32
	*componenttest.FakeRunner
}

type appliedRelease struct {
	chartContent string
	values       map[string]any
}

func newPreviousKube(t *testing.T, chartContent, valuesContent string) *previousKube {
	t.Helper()
	k := &previousKube{chartContent: chartContent, valuesContent: valuesContent, backendReady: 1}
	k.FakeRunner = &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.Contains(command, "get helmchart ") && strings.Contains(command, "chartContent"):
			return sshx.Result{Stdout: k.chartContent}, nil
		case strings.Contains(command, "get helmchart ") && strings.Contains(command, "valuesContent"):
			return sshx.Result{Stdout: k.valuesContent}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl get secret "):
			k.mu.Lock()
			defer k.mu.Unlock()
			if k.secret == nil {
				return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): secrets "` + PreviousReleaseSecret + `" not found`}, nil
			}
			data := map[string]string{}
			for key, value := range k.secret {
				data[key] = base64.StdEncoding.EncodeToString([]byte(value))
			}
			body, err := json.Marshal(map[string]any{
				"apiVersion": "v1", "kind": "Secret",
				"metadata": map[string]any{"name": PreviousReleaseSecret, "namespace": Namespace},
				"data":     data,
			})
			if err != nil {
				return sshx.Result{}, err
			}
			return sshx.Result{Stdout: string(body)}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl apply -f -"):
			stdin := k.lastInput(t)
			var object map[string]any
			if err := yaml.Unmarshal(stdin, &object); err != nil {
				return sshx.Result{}, err
			}
			kind, _ := object["kind"].(string)
			if kind != "Secret" {
				return sshx.Result{}, nil
			}
			data, _ := object["data"].(map[string]any)
			k.mu.Lock()
			k.secret = map[string]string{}
			for key, value := range data {
				encoded, _ := value.(string)
				decoded, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil {
					k.mu.Unlock()
					return sshx.Result{}, err
				}
				k.secret[key] = string(decoded)
			}
			k.mu.Unlock()
			return sshx.Result{Stdout: "secret/" + PreviousReleaseSecret + " configured"}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl delete secret "):
			k.mu.Lock()
			k.secret = nil
			k.mu.Unlock()
			return sshx.Result{}, nil
		case strings.HasPrefix(command, "sudo -n install -m 0600 ") && strings.Contains(command, ReleaseName+".yaml"):
			k.recordApply(t, k.lastInput(t))
			return sshx.Result{}, nil
		case strings.HasPrefix(command, "sudo -n install -m 0600 "):
			return sshx.Result{}, nil
		case strings.Contains(command, "get deployment/"):
			revision := k.chartRevision
			// backendReady is how many replicas the fake reports READY: a
			// restore onto a cluster whose database is gone produces pods that
			// roll out and stay NotReady, which is the case the restore's wait
			// must not confuse with "nothing runs".
			return sshx.Result{Stdout: fmt.Sprintf(`{"metadata":{"generation":1},"spec":{"replicas":1,"template":{"metadata":{"annotations":{"kubenest.io/install-revision":%q}}}},"status":{"observedGeneration":1,"replicas":1,"updatedReplicas":1,"availableReplicas":%d}}`, revision, k.backendReady)}, nil
		case strings.HasPrefix(command, "get statefulset "):
			return sshx.Result{Stdout: `{"spec":{"replicas":1},"status":{"readyReplicas":1}}`}, nil
		case strings.Contains(command, "get certificate/") || strings.Contains(command, "get gateway/"):
			return sshx.Result{Stdout: `{"status":{"conditions":[{"type":"Ready","status":"True"},{"type":"Programmed","status":"True"}]}}`}, nil
		case command == migrationJobCmd:
			return sshx.Result{Stdout: jobJSON(t, "Failed", k.chartRevision)}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl delete job "):
			return sshx.Result{}, nil
		default:
			return sshx.Result{}, nil
		}
	}}
	return k
}

func (k *previousKube) lastInput(t *testing.T) []byte {
	t.Helper()
	inputs := k.Inputs()
	if len(inputs) == 0 {
		t.Fatal("a document was written without streaming one")
	}
	return inputs[len(inputs)-1]
}

func (k *previousKube) recordApply(t *testing.T, manifest []byte) {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(manifest, &doc); err != nil {
		t.Fatalf("the applied manifest is not valid YAML: %v", err)
	}
	spec, _ := doc["spec"].(map[string]any)
	content, _ := spec["chartContent"].(string)
	raw, _ := spec["valuesContent"].(string)
	var values map[string]any
	if err := yaml.Unmarshal([]byte(raw), &values); err != nil {
		t.Fatalf("the applied values are not valid YAML: %v", err)
	}
	if revision, ok := values["installRevision"].(string); ok {
		k.chartRevision = revision
	}
	k.mu.Lock()
	k.applied = append(k.applied, appliedRelease{chartContent: content, values: values})
	k.mu.Unlock()
}

func (k *previousKube) lastApply(t *testing.T) appliedRelease {
	t.Helper()
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.applied) == 0 {
		t.Fatal("no chart was applied")
	}
	return k.applied[len(k.applied)-1]
}

// A FAILED MIGRATION PUTS THE PREVIOUS CHART BACK, with the fence still up.
func TestAFailedMigrationPutsThePreviousChartBackBehindTheFence(t *testing.T) {
	ctx := context.Background()
	previousValues := "domain: kn.example.com\njwtSecret: s\nbackend:\n  admin:\n    email: a@b.c\n"
	// The HelmChart carries the archive BASE64-encoded, which is the shape the
	// capture decodes.
	kube := newPreviousKube(t, base64.StdEncoding.EncodeToString([]byte(previousChartContent)), previousValues)

	// What the fence stage captured and stored before the first apply.
	previous, err := CapturePreviousRelease(ctx, kube, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := StorePreviousRelease(ctx, kube, previous); err != nil {
		t.Fatal(err)
	}

	s := testUpgradeSession(t, kube, previousValues)
	s.PollInterval = time.Millisecond
	s.WaitDeadline = 2 * time.Second
	s.Opts.Before = api.ControlPlaneVersion{Contract: 3, Build: "c121ed88"}

	err = stageMigration(ctx, s)
	if err == nil {
		t.Fatal("a migration Job that failed did not fail the stage")
	}
	if !strings.Contains(err.Error(), MigrationJobName) {
		t.Errorf("the failure does not name the migration Job:\n%v", err)
	}

	// THE PREVIOUS RELEASE IS WHAT RUNS: the last apply carries the archive and
	// the values the control plane ran before this upgrade...
	applied := kube.lastApply(t)
	if applied.chartContent != base64.StdEncoding.EncodeToString([]byte(previousChartContent)) {
		t.Errorf("the last apply carried chart %q, want the PREVIOUS chart: after a failed migration the control plane must run the code its schema matches", applied.chartContent)
	}
	if wrote := applied.values["jwtSecret"]; wrote != "s" {
		t.Errorf("the last apply carried values %v, want the previous release's", applied.values)
	}
	// ...with the FENCE UP, so nothing reaches the rolled-back backend...
	fence, _ := applied.values["fence"].(map[string]any)
	if fence == nil || fence["enabled"] != true {
		t.Errorf("the restored release has fence = %v, want it still up: lowering the fence here would expose a backend nobody has validated", applied.values["fence"])
	}
	// ...and the migration OFF, because the chart has ONE backend image for the
	// Job and the Deployment and the Job's pod template is immutable.
	if migration, ok := applied.values["migration"].(map[string]any); ok && migration["enabled"] == true {
		t.Errorf("the restored release re-enabled the migration Job: helm would patch an immutable pod template onto the Job the failed attempt left")
	}
}

// THE CAPTURE IS DURABLE: the process that raised the fence may be gone.
func TestThePreviousReleaseSurvivesANewProcess(t *testing.T) {
	ctx := context.Background()
	previousValues := "domain: kn.example.com\njwtSecret: secret-value\n"
	writer := newPreviousKube(t, base64.StdEncoding.EncodeToString([]byte(previousChartContent)), previousValues)

	previous, err := CapturePreviousRelease(ctx, writer, 3)
	if err != nil {
		t.Fatal(err)
	}
	if previous.ChartContent != previousChartContent || previous.ValuesContent != previousValues {
		t.Fatalf("the capture is %+v, want the live HelmChart's archive and values", previous)
	}
	if previous.Replicas != 3 {
		t.Errorf("the capture records %d replicas, want the count the backend ran at", previous.Replicas)
	}
	if err := StorePreviousRelease(ctx, writer, previous); err != nil {
		t.Fatal(err)
	}

	// A NEW PROCESS: a fresh fake with no memory of the capture, reading what
	// the cluster holds. That is what a second laptop — or the same laptop
	// after a crash — has.
	reader := newPreviousKube(t, "", "")
	reader.secret = writer.secret
	got, ok, err := ReadPreviousRelease(ctx, reader)
	if err != nil || !ok {
		t.Fatalf("a new process could not read the previous release (ok=%v): %v", ok, err)
	}
	if got != previous {
		t.Errorf("the new process read %+v, want %+v", got, previous)
	}
	// AND WITH NO FENCE THERE IS NOTHING TO READ, which is the ordinary state:
	// it must not be an error, and it must not invent one.
	empty := newPreviousKube(t, "", "")
	if _, ok, err := ReadPreviousRelease(ctx, empty); err != nil || ok {
		t.Errorf("with no fence the read answered ok=%v err=%v, want nothing and no error", ok, err)
	}
}

// The Secret is inside the API server's object limit, which is why the archive
// can travel in one.
func TestTheCapturedPreviousReleaseFitsInASecret(t *testing.T) {
	ctx := context.Background()
	writer := newPreviousKube(t, base64.StdEncoding.EncodeToString(ChartArchive()), "domain: x\n")
	previous, err := CapturePreviousRelease(ctx, writer, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := StorePreviousRelease(ctx, writer, previous); err != nil {
		t.Fatal(err)
	}
	encoded := 0
	for _, value := range writer.secret {
		encoded += base64.StdEncoding.EncodedLen(len(value))
	}
	if encoded > 1<<20 {
		t.Errorf("the captured release encodes to %d bytes, over the API server's 1 MiB object limit", encoded)
	}
	t.Logf("the captured release is %d bytes encoded, against a 1 MiB limit", encoded)
}

// A BACKEND THAT ROLLS OUT BUT NEVER BECOMES READY STILL COUNTS AS RESTORED.
//
// The migration failed because PostgreSQL went away, so the previous backend's
// pods start against a database that is not there: they roll out at the previous
// revision, run the previous image, and sit NotReady. A restore that waited for
// readiness would time out and report "the control plane is running nothing"
// about a control plane whose previous chart IS what runs.
func TestARestoreCountsARolledOutBackendThatIsNotReady(t *testing.T) {
	ctx := context.Background()
	previousValues := "domain: kn.example.com\njwtSecret: s\n"
	kube := newPreviousKube(t, base64.StdEncoding.EncodeToString([]byte(previousChartContent)), previousValues)
	kube.backendReady = 0 // the database is gone

	previous, err := CapturePreviousRelease(ctx, kube, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := StorePreviousRelease(ctx, kube, previous); err != nil {
		t.Fatal(err)
	}

	s := testUpgradeSession(t, kube, previousValues)
	s.PollInterval = time.Millisecond
	s.WaitDeadline = 2 * time.Second
	s.Opts.Before = api.ControlPlaneVersion{Contract: 3, Build: "c121ed88"}

	err = stageMigration(ctx, s)
	if err == nil {
		t.Fatal("a failed migration did not fail the stage")
	}
	if s.Restored == nil {
		t.Fatalf("the restore did not happen, so the previous chart is not what runs:\n%v", err)
	}
	if !s.Restored.RolledOut() {
		t.Fatalf("the restore reports %+v, want the backend rolled out at the previous revision", *s.Restored)
	}
	if s.Restored.Ready != 0 {
		t.Fatalf("the fake reports %d ready replicas, so this arm is not exercising the unready case", s.Restored.Ready)
	}
	// THE MESSAGE SAYS BOTH THINGS: the chart is back, and readiness is a
	// separate observation with its own reason.
	if strings.Contains(err.Error(), "running nothing") {
		t.Errorf("the failure tells the operator the control plane is running nothing, although the previous chart was restored:\n%v", err)
	}
	if !strings.Contains(err.Error(), "not Ready") {
		t.Errorf("the failure does not say the restored backend is not Ready yet:\n%v", err)
	}
	if applied := kube.lastApply(t); applied.chartContent != base64.StdEncoding.EncodeToString([]byte(previousChartContent)) {
		t.Errorf("the last apply did not carry the previous chart: %v", applied.chartContent)
	}
}
