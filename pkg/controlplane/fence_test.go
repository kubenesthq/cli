package controlplane

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/sshx"
)

// fenceValues is the values document the fence is raised over.
const fenceTestValues = "domain: kn.example.com\njwtSecret: s\nagentJwtSecret: a\nencryptionKey: e\n"

// newFenceRunner is a FakeRunner answering the reads Raise makes and recording
// everything it is asked to do.
func newFenceRunner(serviceExists bool) *componenttest.FakeRunner {
	return &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.Contains(command, backendDeploymentImageCmd):
			return sshx.Result{Stdout: "ghcr.io/kubenesthq/kubenest-backend:132b7ea"}, nil
		case strings.Contains(command, "get service "+FenceService):
			if serviceExists {
				return sshx.Result{Stdout: FenceService}, nil
			}
			return sshx.Result{ExitCode: 1, Stderr: "not found"}, nil
		default:
			return sshx.Result{}, nil
		}
	}}
}

// fenceObjectsOf parses the multi-document manifest the fence is written as.
func fenceObjectsOf(t *testing.T, write componenttest.Execution) []map[string]any {
	t.Helper()
	var objects []map[string]any
	decoder := yaml.NewDecoder(strings.NewReader(string(write.Stdin)))
	for {
		var doc map[string]any
		if err := decoder.Decode(&doc); err != nil {
			if strings.Contains(err.Error(), "EOF") {
				break
			}
			t.Fatalf("the fence manifest does not parse: %v\n%s", err, write.Stdin)
		}
		if len(doc) > 0 {
			objects = append(objects, doc)
		}
	}
	if len(objects) == 0 {
		t.Fatal("the fence manifest carries no objects")
	}
	return objects
}

// Raising the fence is what takes the public API away from the backend, and it
// has to leave behind an object that ANSWERS, on every path — not a route
// naming a Service that does not exist, which the data plane refuses to
// resolve. The check runs the whole chain instead of two of its halves: the
// values Raise returns, through the chart's own helper, give the api.<domain>
// route a backendRef; that backendRef must name the Service Raise wrote.
func TestRaiseRepointsTheApiRouteAtA503Service(t *testing.T) {
	ctx := context.Background()
	r := newFenceRunner(true)

	values, err := Raise(ctx, r, fenceTestValues, RaiseOptions{Stamp: "test-raise", ServiceWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(values), &doc); err != nil {
		t.Fatalf("the values Raise returned do not parse: %v", err)
	}
	fence, _ := doc["fence"].(map[string]any)
	if fence == nil || fence["enabled"] != true {
		t.Fatalf("Raise returned values with fence = %v, want enabled: true", doc["fence"])
	}

	// The fence's objects: a Service the chart's route can point at, and a
	// Deployment that answers 503 on every path.
	var (
		manifest  componenttest.Execution
		svcObject map[string]any
	)
	for _, run := range r.Executions() {
		if !strings.Contains(run.Command, fenceName+".yaml") {
			continue
		}
		manifest = run
	}
	if manifest.Command == "" {
		t.Fatalf("Raise wrote no fence manifest; commands were %v", r.Commands())
	}
	objects := fenceObjectsOf(t, manifest)
	service := fenceObject(t, objects, "Service")
	if name := fenceDigString(t, service, "metadata", "name"); name != FenceService {
		t.Errorf("the fence Service is named %q, want %q", name, FenceService)
	}
	if port := fenceDigInt(t, service, "spec", "ports", "0", "port"); port != fencePort {
		t.Errorf("the fence Service publishes port %d, want %d", port, fencePort)
	}
	svcObject = service

	deployment := fenceObject(t, objects, "Deployment")
	selector := fenceDigMap(t, deployment, "spec", "selector", "matchLabels")
	podLabels := fenceDigMap(t, deployment, "spec", "template", "metadata", "labels")
	for key, want := range selector {
		if podLabels[key] != want {
			t.Errorf("the fence Deployment selects %s=%v but its pods carry %v, so the Service never gains an endpoint",
				key, want, podLabels[key])
		}
	}
	if got := fenceDigMap(t, svcObject, "spec", "selector"); got["app.kubernetes.io/name"] != podLabels["app.kubernetes.io/name"] {
		t.Errorf("the fence Service selects a different pod than the fence Deployment creates: %v vs %v",
			got, podLabels)
	}
	command := fenceDigString(t, deployment, "spec", "template", "spec", "containers", "0", "command", "3")
	for _, method := range []string{"do_GET", "do_POST", "do_PUT", "do_PATCH", "do_DELETE"} {
		if !strings.Contains(command, method) {
			t.Errorf("the fence's 503 server does not answer %s, so that method would not be fenced:\n%s", method, command)
		}
	}
	if !strings.Contains(command, "send_response(503)") {
		t.Errorf("the fence's server does not answer 503:\n%s", command)
	}
	if !strings.Contains(command, "FENCE_MESSAGE") {
		t.Errorf("the fence's server does not serve the ConfigMap's message, so the customer sees no explanation:\n%s", command)
	}
	if fenceObject(t, objects, "ConfigMap") == nil {
		t.Fatal("the fence has no ConfigMap, so the message the Deployment serves has no source")
	}

	// And the half only the chart can prove: that the value Raise returned
	// makes the api.<domain> route point at that Service. Skipped when helm or
	// the sibling chart is not here.
	chartRoot := siblingChartRoot(t)
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skipf("helm is not installed, so the rendered route cannot be read here: %v", err)
	}
	out, err := exec.Command("helm", "template", ReleaseName, chartRoot,
		"--set", "jwtSecret=x", "--set", "agentJwtSecret=y", "--set", "encryptionKey=z",
		"--set", "backend.admin.password=p", "--set", "gatewayCA.certificate=c",
		"--set", "gatewayCA.privateKey=k", "--set", "checkpoint.enabled=false",
		"--set", "fence.enabled=true").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	apiRef := renderedAPIBackendRef(t, string(out))
	if apiRef.name != FenceService || apiRef.port != fencePort {
		t.Errorf("with the values Raise returned, the api route points at %s:%d, want %s:%d",
			apiRef.name, apiRef.port, FenceService, fencePort)
	}
}

// Lowering the fence restores the route to the backend and removes the fence's
// objects — AFTER the restore has been applied, because the other order leaves
// the route pointing at a Service that no longer exists.
func TestLowerRestoresTheBackendRefAndDeletesTheFence(t *testing.T) {
	ctx := context.Background()
	r := newFenceRunner(true)
	replicas := int32(1)

	var applied string
	var confirmed bool
	err := Lower(ctx, r, fenceTestValues, &replicas, func(values string) error {
		applied = values
		return nil
	}, func() error {
		confirmed = true
		return nil
	})
	if !confirmed {
		t.Fatal("Lower deleted the fence without confirming the route had been restored, so api.<domain> named a Service that was gone")
	}
	if err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(applied), &doc); err != nil {
		t.Fatalf("the values Lower applied do not parse: %v", err)
	}
	fence, _ := doc["fence"].(map[string]any)
	if fence == nil || fence["enabled"] == true {
		t.Fatalf("Lower applied values with fence = %v, want it disabled", doc["fence"])
	}
	backend, _ := doc["backend"].(map[string]any)
	if backend == nil || backend["replicas"] != 1 {
		t.Fatalf("Lower applied backend = %v, want the replica count restored to 1", doc["backend"])
	}

	var deleted, removed bool
	for _, command := range r.Commands() {
		if strings.Contains(command, "delete deployment/"+fenceName) &&
			strings.Contains(command, "service/"+fenceName) &&
			strings.Contains(command, "configmap/"+fenceName) &&
			strings.Contains(command, "--ignore-not-found") {
			deleted = true
		}
		if strings.Contains(command, "rm -f") && strings.Contains(command, fenceName+".yaml") {
			removed = true
		}
	}
	if !deleted {
		t.Errorf("Lower did not delete the fence's objects; commands were %v", r.Commands())
	}
	if !removed {
		t.Errorf("Lower left the fence's durable manifest in k3s's auto-deploy directory, so the fence comes back on the next server start; commands were %v", r.Commands())
	}
}

// The hub carries agent connections through the whole upgrade: an upgrade that
// fenced or scaled it would disconnect every cluster at exactly the moment
// their reports matter most.
func TestTheHubIsNeverScaledOrFenced(t *testing.T) {
	ctx := context.Background()
	r := newFenceRunner(true)

	if _, err := Raise(ctx, r, fenceTestValues, RaiseOptions{Stamp: "test-raise", ServiceWait: time.Second}); err != nil {
		t.Fatal(err)
	}
	for _, run := range r.Executions() {
		if !strings.Contains(run.Command, fenceName+".yaml") {
			continue
		}
		for _, object := range fenceObjectsOf(t, run) {
			kind, _ := object["kind"].(string)
			metadata, _ := object["metadata"].(map[string]any)
			name, _ := metadata["name"].(string)
			if strings.Contains(name, "hub") {
				t.Errorf("the fence writes a %s named %q: the hub must keep running so agents stay connected", kind, name)
			}
		}
	}

	values, err := FenceValues(fenceTestValues, FenceOptions{Up: true, BackendReplicas: fenceInt32(0)})
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]any
	if err := yaml.Unmarshal([]byte(fenceTestValues), &before); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(values), &after); err != nil {
		t.Fatal(err)
	}
	// The only groups the fence may change are the fence's and the backend's
	// replica count. Anything else touched here is this step deciding to stop
	// or divert a workload it was not asked about.
	delete(before, "fence")
	delete(before, "backend")
	delete(after, "fence")
	afterBackend, _ := after["backend"].(map[string]any)
	delete(afterBackend, "replicas")
	delete(after, "backend")
	if len(before) != len(after) {
		t.Fatalf("the fence changed groups it has no business changing: before %v, after %v", before, after)
	}
	for key, want := range before {
		if got := after[key]; !fenceSameValue(got, want) {
			t.Errorf("the fence changed %s from %v to %v", key, want, got)
		}
	}
	if _, ok := after["hub"]; ok {
		t.Error("the fence's values carry a hub group: the hub is not the fence's to configure")
	}
}

func fenceInt32(v int32) *int32 { return &v }

func fenceSameValue(a, b any) bool {
	ja, _ := yaml.Marshal(a)
	jb, _ := yaml.Marshal(b)
	return string(ja) == string(jb)
}

func fenceObject(t *testing.T, objects []map[string]any, kind string) map[string]any {
	t.Helper()
	for _, object := range objects {
		if object["kind"] == kind {
			return object
		}
	}
	t.Fatalf("the fence manifest has no %s", kind)
	return nil
}

// fenceDigMap walks nested maps, failing the test when the path is absent.
func fenceDigMap(t *testing.T, object map[string]any, path ...string) map[string]any {
	t.Helper()
	value := fenceDigAny(t, object, path...)
	got, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%v is %T, not a mapping", path, value)
	}
	return got
}

func fenceDigString(t *testing.T, object map[string]any, path ...string) string {
	t.Helper()
	value := fenceDigAny(t, object, path...)
	got, ok := value.(string)
	if !ok {
		t.Fatalf("%v is %T, not a string", path, value)
	}
	return got
}

func fenceDigInt(t *testing.T, object map[string]any, path ...string) int {
	t.Helper()
	value := fenceDigAny(t, object, path...)
	got, ok := value.(int)
	if !ok {
		t.Fatalf("%v is %T, not an int", path, value)
	}
	return got
}

func fenceDigAny(t *testing.T, object map[string]any, path ...string) any {
	t.Helper()
	var current any = object
	for _, key := range path {
		switch node := current.(type) {
		case map[string]any:
			value, ok := node[key]
			if !ok {
				t.Fatalf("%v is missing on the way to %v", key, path)
			}
			current = value
		case []any:
			index, err := strconv.Atoi(key)
			if err != nil {
				t.Fatalf("%v indexes a list with %q", path, key)
			}
			if index >= len(node) {
				t.Fatalf("%v is out of range on the way to %v", key, path)
			}
			current = node[index]
		default:
			t.Fatalf("%v is %T on the way to %v", key, current, path)
		}
	}
	return current
}

// renderedAPIBackendRef reads the api.<domain> HTTPRoute's backendRef out of a
// rendered chart.
func renderedAPIBackendRef(t *testing.T, rendered string) struct {
	name string
	port int
} {
	t.Helper()
	out := struct {
		name string
		port int
	}{}
	decoder := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var doc map[string]any
		if err := decoder.Decode(&doc); err != nil {
			break
		}
		if doc["kind"] != "HTTPRoute" {
			continue
		}
		metadata, _ := doc["metadata"].(map[string]any)
		if metadata == nil || metadata["name"] != ReleaseName+"-api" {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		rules, _ := spec["rules"].([]any)
		if len(rules) == 0 {
			t.Fatal("the api route has no rules")
		}
		rule, _ := rules[0].(map[string]any)
		refs, _ := rule["backendRefs"].([]any)
		if len(refs) != 1 {
			t.Fatalf("the api route has %d backendRefs, want exactly 1", len(refs))
		}
		ref, _ := refs[0].(map[string]any)
		out.name, _ = ref["name"].(string)
		out.port, _ = ref["port"].(int)
		return out
	}
	t.Fatalf("the rendered chart carries no HTTPRoute named %s-api", ReleaseName)
	return out
}

// The pin the fence applies is the image the CHART RENDERS, not only the value
// the CLI writes. Helm merges the values over the chart's defaults, and the
// chart's default backend.image carries the NEW release's digest, which the
// helper renders whenever it is set. A pin that omits a key leaves the default
// in force, so this is read from `helm template` rather than from the values.
// Skipped when helm or the sibling chart is not here.
func TestThePinnedRunningImageIsWhatTheChartRenders(t *testing.T) {
	chartRoot := siblingChartRoot(t)
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skipf("helm is not installed, so the rendered Deployment cannot be read here: %v", err)
	}
	const repo = "ghcr.io/kubenesthq/kubenest-backend"
	const digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	base := fenceTestValues + "backend:\n  admin:\n    password: p\ngatewayCA:\n  certificate: c\n  privateKey: k\ncheckpoint:\n  enabled: false\n"
	for _, tc := range []struct{ running, want string }{
		{repo + "@" + digest, repo + "@" + digest},
		{repo + ":675ff0e", repo + ":675ff0e"},
		{repo + ":675ff0e@" + digest, repo + "@" + digest},
	} {
		image, err := ParseBackendImage(tc.running)
		if err != nil {
			t.Fatalf("%s: %v", tc.running, err)
		}
		values, err := FenceValues(base, FenceOptions{Up: true, MigrationOff: true, HeldImage: &image})
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(file, []byte(values), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("helm", "template", ReleaseName, chartRoot, "-f", file).CombinedOutput()
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, out)
		}
		if got := renderedBackendImage(t, string(out)); got != tc.want {
			t.Errorf("running %s: the fence apply renders the backend at %s, want %s — the apply would roll the backend before the checkpoint",
				tc.running, got, tc.want)
		}
	}
}

// renderedBackendImage reads the backend Deployment's container image out of a
// rendered chart.
func renderedBackendImage(t *testing.T, rendered string) string {
	t.Helper()
	decoder := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var doc map[string]any
		if err := decoder.Decode(&doc); err != nil {
			break
		}
		metadata, _ := doc["metadata"].(map[string]any)
		if doc["kind"] != "Deployment" || metadata == nil || metadata["name"] != backendService {
			continue
		}
		return fenceDigString(t, doc, "spec", "template", "spec", "containers", "0", "image")
	}
	t.Fatalf("the rendered chart carries no Deployment named %s", backendService)
	return ""
}

// The fence answers 503 to every HTTP request, so an HTTP readiness probe can
// never pass: on hardware (2026-09-25) the fence pod ran and served its page,
// was never Ready, and the upgrade stopped after the component-ready deadline
// waiting for it. Earlier runs had hidden this, because a route resting on a
// Service with no ready endpoints ALSO answers 503. Readiness must ask whether
// the fence is listening, not what it answers.
func TestTheFenceCanBecomeReadyWhileAnsweringOnly503(t *testing.T) {
	r := newFenceRunner(true)
	if _, err := Raise(context.Background(), r, fenceTestValues, RaiseOptions{Stamp: "test-raise", ServiceWait: time.Second}); err != nil {
		t.Fatal(err)
	}
	var manifest componenttest.Execution
	for _, run := range r.Executions() {
		if strings.Contains(run.Command, fenceName+".yaml") {
			manifest = run
		}
	}
	deployment := fenceObject(t, fenceObjectsOf(t, manifest), "Deployment")
	probe := fenceDigMap(t, deployment, "spec", "template", "spec", "containers", "0", "readinessProbe")
	if _, http := probe["httpGet"]; http {
		t.Fatalf("the fence's readiness probe is an HTTP GET, which only passes on 2xx/3xx, and the fence answers 503 to everything: %v", probe)
	}
	tcp, _ := probe["tcpSocket"].(map[string]any)
	if tcp == nil || fmt.Sprint(tcp["port"]) != "http" && fmt.Sprint(tcp["port"]) != fmt.Sprint(fencePort) {
		t.Errorf("the fence's readiness probe does not check that the fence is listening on its port: %v", probe)
	}
}
