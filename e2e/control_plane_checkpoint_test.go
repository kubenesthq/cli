//go:build e2e

// T4.7's real-hardware gate: the control plane's own recovery points, on the
// all-in-one fixture.
//
// THE FIXTURE IS AN ALREADY-INSTALLED ALL-IN-ONE CONTROL PLANE. This gate never
// installs one: KUBENEST_LAB_SERVER_IP is its host, and KUBENEST_CONTROL_PLANE /
// KUBENEST_CLI_TOKEN are that control plane. Installing is T4.6's gate; what is
// under test here is that the control plane can be backed up on demand, that a
// security change is covered by a checkpoint, and that a stopped checkpoint
// CronJob is visible in the management cluster's `backup` verdict.
//
// What it asserts:
//
//	(a) `kubenest backup now --control-plane`, run through the real command
//	    tree, creates a Job FROM the chart's CronJob — same image, same service
//	    account, the chart's scratch volume — and returns only once a NEW
//	    checkpoint is eligible. The wall-clock time and the sealed checkpoint's
//	    size are recorded, and the size is checked to be non-zero: a checkpoint
//	    of a real database is not an empty object.
//	(b) revoking a CLI token through the API — minted first, because a CLI
//	    token cannot mint or revoke another; those endpoints take a user session
//	    — makes a checkpoint that CONTAINS the change eligible within 60 s, and
//	    the control plane stops reporting the change as unprotected.
//	(c) suspending the CronJob is visible: the management cluster's `backup`
//	    group reports the newest eligible checkpoint as STALE. The threshold is
//	    48 h, so the test rewinds the checkpoint's published timestamp — which is
//	    exactly what 48 hours of a stopped CronJob produces — and the suspension
//	    is what stops the next run from overwriting it. One report carries it, so
//	    the wait is bounded by the agent's own cadence.
//	(d) the nightly retention and the 14-day floor are read from the manifest
//	    fields — the CronJob the chart installed and the checkpoint's own
//	    published retention — rather than by waiting fourteen days.
//
// WHERE THE BACKEND'S ANSWER COMES FROM. There is no `kubenest health` command
// yet (kn-9pgx builds it), so (b) and (c) read the fleet-health API the CLI's
// own client talks to, with the token the CLI holds. The management cluster is
// identified by the backend's own record — the control-plane recovery kit names
// the cluster the install ran in — which is the same source the backend's fold
// uses, so this gate cannot pass by asking about a different cluster.
//
// Run from the umbrella workspace:
//
//	source lab/hetzner/.lab-env.sh
//	export KUBENEST_LAB_SERVER_IP=<the control plane's host>
//	export KUBENEST_CONTROL_PLANE=https://api.<domain>
//	export KUBENEST_CLI_TOKEN=knp_...
//	export KUBENEST_ADMIN_EMAIL=admin@<domain> KUBENEST_ADMIN_PASSWORD=...   # the install printed it
//	cd kubenest-cli && go test -tags e2e -v -timeout 40m ./e2e/ -run TestControlPlaneCheckpointGate
//
// Pass limits, from the bead: the revocation-triggered checkpoint is eligible
// within 60 s of the change; the on-demand checkpoint within the bundle's
// component-ready-bounded wait; the CronJob's suspension shows up in the next
// health report.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/cmd"
	"kubenest.io/cli/pkg/controlplane"
	"kubenest.io/cli/pkg/k3s"
)

// t47RetentionFloor is the plan's floor for a nightly checkpoint's objects
// (kubenest-helm/kubenest/values.yaml: "THE FLOOR IS 14 DAYS"): a shorter
// retention cannot be handed to a customer as a recovery history. It is
// asserted against the chart's own fields rather than by waiting for objects to
// expire.
const t47RetentionFloor = 14 * 24 * 3600

// t47HealthReportWait bounds the wait for a health report to carry a change.
// The agent reports every 60 s and each report is evaluated when it lands, so
// three intervals is a verdict rather than an optimistic guess.
const t47HealthReportWait = 3 * time.Minute

// t47Env is the fixture this gate needs.
type t47Env struct {
	gateEnv
	adminEmail    string
	adminPassword string
}

func t47Environment(t *testing.T) t47Env {
	t.Helper()
	return t47Env{
		gateEnv: gateEnvironment(t),
		// THE CONTROL PLANE'S OWN ADMINISTRATOR, created by the install that
		// built this fixture. Minting and revoking a CLI token requires a user
		// session (a knp_ token cannot mint or revoke another), so the gate
		// needs the account; the defaults are the ones the repository's own lab
		// scripts use (scripts/addon-install-gate.sh).
		adminEmail:    envOr("KUBENEST_ADMIN_EMAIL", "lakshmi@lakshminp.com"),
		adminPassword: envOr("KUBENEST_ADMIN_PASSWORD", "admin123"),
	}
}

func TestControlPlaneCheckpointGate(t *testing.T) {
	env := t47Environment(t)
	ctx := context.Background()

	// An install of its own is NOT this gate's business: the fixture is the
	// control plane already running on the host.
	runner := operationRunner(t, env.gateEnv)
	cp := t47API{base: strings.TrimRight(env.controlPlane, "/"), token: env.token, http: &http.Client{Timeout: 30 * time.Second}}

	// The command is what is under test, so it runs against a HOME this test
	// owns: the flags, the config file and the refusals are the operator's, and
	// nothing here touches the operator's own state.
	t.Setenv("HOME", t.TempDir())

	// The control plane's own record names the cluster it runs in. That is the
	// backend's source for "which cluster is the management cluster", and the
	// cluster whose `backup` group the assertions below read.
	orgs, err := cp.Orgs(ctx)
	if err != nil {
		t.Fatalf("reading the organizations: %v", err)
	}
	if len(orgs) == 0 {
		t.Fatal("the control plane holds no organization: the fixture is not an installed control plane")
	}
	session := cp.Auth(ctx, t, env.adminEmail, env.adminPassword)
	managementCluster := cp.ManagementCluster(ctx, t, session, orgs[0].ID)
	t.Logf("the control plane runs in cluster %s", managementCluster)

	// The manifest the run reads its deadline from, at the version the control
	// plane has recorded for that cluster.
	bundlePath := t47BundleManifest(t, ctx, env, managementCluster)

	// What is eligible before anything this gate does: every later "a new
	// checkpoint exists" is measured against this.
	before, err := controlplane.ReadEligibleCheckpoint(ctx, runner)
	if err != nil {
		t.Fatalf("reading the checkpoint status before the gate: %v", err)
	}
	if before != nil {
		t.Logf("newest eligible checkpoint before the gate: %s (%d bytes, at %s)", before.Key, before.SizeBytes, before.At)
	}

	var onDemand controlplane.EligibleCheckpoint

	t.Run("backup now --control-plane creates the Job from the chart's CronJob and waits for an eligible checkpoint", func(t *testing.T) {
		var out bytes.Buffer
		started := time.Now()
		err := t47RunCLI(&out, t47BackupArgs(env, bundlePath, "--control-plane")...)
		elapsed := time.Since(started)
		if err != nil {
			t.Fatalf("kubenest backup now --control-plane failed after %s: %v\n%s", elapsed.Round(time.Second), err, out.String())
		}

		marker, err := controlplane.ReadEligibleCheckpoint(ctx, runner)
		if err != nil {
			t.Fatal(err)
		}
		if marker == nil {
			t.Fatal("the command reported success and the control plane has no eligible checkpoint")
		}
		if before != nil && marker.Key == before.Key {
			t.Fatalf("the command reported success and the newest eligible checkpoint is still %s: no checkpoint was published", marker.Key)
		}
		if marker.SizeBytes <= 0 {
			t.Errorf("the checkpoint's sealed size is %d bytes: an empty object is not a recovery point", marker.SizeBytes)
		}
		// The wall-clock time and the size are RECORDED: "how long does a
		// checkpoint take, and how big is it" is what sizes the window of loss
		// a recovery has to state.
		t.Logf("on-demand checkpoint %s eligible after %s: %d bytes sealed, dumped at %s, retention %ds, postgres %d (%s)",
			marker.Key, elapsed.Round(time.Second), marker.SizeBytes, marker.At, marker.RetentionSeconds, marker.PostgresMajor, marker.PostgresImage)

		// The Job is the CHART'S checkpoint run, not a hand-rolled one: the
		// same image, the same service account and the chart's own scratch
		// volume. That is what makes an on-demand recovery point the same kind
		// of object as a scheduled one.
		cronjob, err := t47CronJob(ctx, runner)
		if err != nil {
			t.Fatal(err)
		}
		job, err := t47ManualJob(ctx, runner)
		if err != nil {
			t.Fatal(err)
		}
		if job.Metadata.Name == controlplane.CheckpointCronJobName {
			t.Errorf("the on-demand Job is named after the CronJob (%s): a second run would collide with it", job.Metadata.Name)
		}
		if want := cronjob.Spec.JobTemplate.Spec.Template.Spec.ServiceAccountName; job.Spec.Template.Spec.ServiceAccountName != want {
			t.Errorf("the Job runs as %q, want the CronJob's %q: an on-demand checkpoint must not need a different identity",
				job.Spec.Template.Spec.ServiceAccountName, want)
		}
		if want, got := cronjob.image(), job.image(); got != want {
			t.Errorf("the Job runs image %q, want the CronJob's %q: the digest is pinned in the chart, and a Job built elsewhere would drift from it", got, want)
		}
		if want, got := cronjob.scratchClaim(), job.scratchClaim(); got != want {
			t.Errorf("the Job's scratch volume is %q, want the chart's %q: the dump of a customer's control plane does not belong on a node's disk",
				got, want)
		}
		t.Logf("on-demand Job %s: service account %s, image %s, scratch %s",
			job.Metadata.Name, job.Spec.Template.Spec.ServiceAccountName, job.image(), job.scratchClaim())
		onDemand = *marker
	})

	t.Run("a revoked CLI token is covered by a checkpoint within 60 s", func(t *testing.T) {
		pre, err := controlplane.ReadEligibleCheckpoint(ctx, runner)
		if err != nil {
			t.Fatal(err)
		}

		// MINTED FIRST, and it must be minted: the revocation is about a token
		// that existed. The endpoints take a user session, which is what
		// `session` is — a CLI token cannot mint or revoke another.
		change := cp.MintAndRevoke(ctx, t, session, fmt.Sprintf("gate-t47-%d", time.Now().Unix()))
		started := time.Now()

		var covered *controlplane.EligibleCheckpoint
		t47WaitFor(t, 60*time.Second, 2*time.Second,
			"a checkpoint containing the revoked CLI token ("+change+") to become eligible",
			func() (bool, string) {
				marker, err := controlplane.ReadEligibleCheckpoint(ctx, runner)
				if err != nil {
					return false, err.Error()
				}
				if marker == nil {
					return false, "no eligible checkpoint yet"
				}
				if marker.IncludesSecurityChangeAt == "" {
					return false, fmt.Sprintf("%s carries no security change", marker.Key)
				}
				if pre != nil {
					if marker.Key == pre.Key {
						return false, "still " + marker.Key
					}
					// The new marker must claim a change NEWER than the
					// checkpoint that already existed. A nightly or on-demand
					// checkpoint landing in the same window carries no security
					// change and is not evidence that the revocation is
					// protected.
					if !t47After(marker.IncludesSecurityChangeAt, pre.At) {
						return false, fmt.Sprintf("%s includes %s, which is not after the checkpoint that already existed", marker.Key, marker.IncludesSecurityChangeAt)
					}
				}
				covered = marker
				return true, marker.Key + " includes the change at " + marker.IncludesSecurityChangeAt
			})
		t.Logf("the revocation is covered by %s %s after the change: security change at %s, retention %ds",
			covered.Key, time.Since(started).Round(time.Second), covered.IncludesSecurityChangeAt, covered.RetentionSeconds)

		// AND THE CONTROL PLANE SAYS SO. The backend folds the checkpoint's
		// marker into the management cluster's `backup` verdict, so the verdict
		// is what stops naming the change as unprotected.
		t47WaitFor(t, t47HealthReportWait, 5*time.Second,
			"the management cluster's backup verdict to stop reporting the change as unprotected",
			func() (bool, string) {
				return t47BackupVerdict(ctx, t, cp, managementCluster, func(check t47Check, evidence map[string]any) (bool, string) {
					if unprotected := t47Unprotected(evidence); len(unprotected) > 0 {
						return false, fmt.Sprintf("%s: %s", check.ReasonCode, check.Message)
					}
					return true, fmt.Sprintf("%s: %s (checkpoint %v)", check.ReasonCode, check.Message, evidence["latest_eligible_key"])
				})
			})
	})

	// The weekly restore drill, run once from its own CronJob. A fresh fixture has
	// never drilled, and "checkpoints exist but none has ever been restored" is a
	// warning of its own that outranks the staleness the next arm is about (the
	// third hardware run of this gate saw exactly that), so the drill runs first:
	// it is also the plan's "the weekly drill's result appears in the management
	// cluster's backup group".
	t.Run("the weekly drill restores the newest checkpoint and its result is published", func(t *testing.T) {
		job := "kubenest-cp-checkpoint-drill-gate-" + strconv.FormatInt(time.Now().Unix(), 10)
		res, err := runner.Run(ctx, "sudo -n k3s kubectl create job "+job+" --from=cronjob/"+controlplane.CheckpointCronJobName+"-drill -n "+controlplane.Namespace)
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("creating the drill Job from its CronJob: %v / exit %d: %s", err, res.ExitCode, res.Stderr)
		}
		t.Cleanup(func() {
			_, _ = runner.Run(context.Background(), "sudo -n k3s kubectl delete job "+job+" -n "+controlplane.Namespace+" --wait=false")
		})
		// The drill decrypts the checkpoint with the identity Secret the chart
		// mounts; without it the pod can never start, which is a finding to
		// name at once rather than fifteen minutes of "Pending".
		if _, err := k3s.Kubectl(ctx, runner, "get secret kubenest-cp-checkpoint-identity -n "+controlplane.Namespace+" -o name"); err != nil {
			t.Fatalf("the drill's identity Secret kubenest-cp-checkpoint-identity does not exist, so its pod cannot mount it and never starts (kn-drill-identity-u6il): %v", err)
		}
		started := time.Now()
		t47WaitFor(t, 15*time.Minute, 10*time.Second, "the drill Job to finish", func() (bool, string) {
			out, err := k3s.Kubectl(ctx, runner, "get job "+job+" -n "+controlplane.Namespace+" -o jsonpath={.status.conditions[*].type}")
			if err != nil {
				return false, err.Error()
			}
			if strings.Contains(out, "Failed") {
				t.Fatalf("the drill Job failed: read it with `kubectl logs job/%s -n %s`", job, controlplane.Namespace)
			}
			return strings.Contains(out, "Complete"), "conditions: " + out
		})
		document, err := t47StatusDocument(ctx, runner)
		if err != nil {
			t.Fatal(err)
		}
		drill, _ := document["drill"].(map[string]any)
		if drill["status"] != "passed" {
			t.Fatalf("the drill finished but published %v, want status passed", drill)
		}
		t.Logf("drill of %v passed in %s (row counts matched: %v)", drill["checkpoint"], time.Since(started).Round(time.Second), drill["row_counts_matched"])
	})

	t.Run("a stopped checkpoint CronJob shows up as a stale checkpoint", func(t *testing.T) {
		// The CronJob is SUSPENDED so the rewound timestamp cannot be
		// overwritten by the next scheduled run while this subtest watches —
		// which is the same statement a customer makes when they stop it.
		t47Suspend(ctx, t, runner, true)

		document, err := t47StatusDocument(ctx, runner)
		if err != nil {
			t.Fatal(err)
		}
		// The original is kept apart from the rewound copy: the first version
		// of this gate rewound the document in place, never wrote it, and then
		// "restored" the rewound copy in its cleanup, so the wait watched an
		// unchanged status and the fixture was left 49 hours in the past.
		original, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// Leave the fixture as it was found: the timestamp back, the CronJob
			// resumed. A gate that left a rewound status behind would leave the
			// next reader with a lie.
			var restore map[string]any
			if err := json.Unmarshal(original, &restore); err == nil {
				if err := t47WriteStatus(context.Background(), runner, restore); err != nil {
					t.Logf("restoring the checkpoint status: %v", err)
				}
			}
			t47Suspend(context.Background(), t, runner, false)
		})
		t47RewindCheckpoint(t, document, 49*time.Hour)
		if err := t47WriteStatus(ctx, runner, document); err != nil {
			t.Fatalf("publishing the rewound status: %v", err)
		}

		// The group is a warning either way (the weekly drill has not run on a
		// fresh fixture), so the assertion is the REASON: with a checkpoint
		// older than 48 hours, the verdict must say the checkpoint is stale.
		t47WaitFor(t, t47HealthReportWait, 5*time.Second,
			"the management cluster's backup verdict to report the newest eligible checkpoint as stale",
			func() (bool, string) {
				return t47BackupVerdict(ctx, t, cp, managementCluster, func(check t47Check, evidence map[string]any) (bool, string) {
					if check.ReasonCode == "CONTROL_PLANE_CHECKPOINT_STALE" {
						return true, fmt.Sprintf("%s (%s): %s", check.Status, check.ReasonCode, check.Message)
					}
					return false, fmt.Sprintf("%s: %s", check.ReasonCode, check.Message)
				})
			})
	})

	t.Run("the nightly retention and the 14-day floor come from the manifest", func(t *testing.T) {
		// READ, NOT WAITED FOR. Object expiry is a function of the chart's own
		// fields, so waiting fourteen days would prove nothing that reading
		// them does not — and the fields are what a chart change has to keep.
		cronjob, err := t47CronJob(ctx, runner)
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(cronjob.Spec.Schedule)
		if len(fields) != 5 {
			t.Fatalf("the checkpoint CronJob's schedule is %q, which is not a cron expression", cronjob.Spec.Schedule)
		}
		if fields[0] == "*" || fields[1] == "*" {
			t.Errorf("the checkpoint CronJob's schedule is %q: a run more often than nightly cannot be retained as a history", cronjob.Spec.Schedule)
		}
		if fields[2] != "*" || fields[3] != "*" || fields[4] != "*" {
			t.Errorf("the checkpoint CronJob's schedule is %q, which does not fire once a day", cronjob.Spec.Schedule)
		}

		ttl := int64(0)
		if cronjob.Spec.JobTemplate.Spec.TTLSecondsAfterFinished != nil {
			ttl = *cronjob.Spec.JobTemplate.Spec.TTLSecondsAfterFinished
		}
		if ttl < t47RetentionFloor {
			t.Errorf("the checkpoint CronJob keeps its Jobs for %d s, below the 14-day floor (%d s)", ttl, t47RetentionFloor)
		}
		nightly := cronjob.retentionSeconds()
		if nightly < t47RetentionFloor {
			t.Errorf("the checkpoint's own CHECKPOINT_RETENTION_SECONDS is %d, below the 14-day floor (%d s): the chart value is what every run stamps into its manifest",
				nightly, t47RetentionFloor)
		}
		if onDemand.RetentionSeconds < t47RetentionFloor {
			t.Errorf("the checkpoint this gate took publishes retention_seconds=%d, below the 14-day floor (%d s): the field a recovery reads must carry the nightly retention, not a security-triggered one",
				onDemand.RetentionSeconds, t47RetentionFloor)
		}
		t.Logf("checkpoint CronJob %s: schedule %q, Job TTL %ds, nightly retention %ds (floor %ds)",
			controlplane.CheckpointCronJobName, cronjob.Spec.Schedule, ttl, nightly, t47RetentionFloor)
	})
}

// t47RunCLI runs the real command tree, so the flags and the refusals are the
// ones an operator gets.
func t47RunCLI(out io.Writer, args ...string) error {
	root := cmd.NewRootCommand()
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	return root.Execute()
}

// t47BackupArgs is the control-plane invocation: the server it reaches and the
// bundle manifest its deadline comes from, and no --cluster — the control plane
// runs in the management cluster the server addresses.
func t47BackupArgs(env t47Env, bundlePath string, extra ...string) []string {
	args := []string{"backup", "now", "--server", env.server, "--bundle-manifest", bundlePath}
	if env.sshUser != "" {
		args = append(args, "--ssh-user", env.sshUser)
	}
	if env.sshKey != "" {
		args = append(args, "--ssh-key", env.sshKey)
	}
	return append(args, extra...)
}

// t47BundleManifest writes the manifest the command bounds its wait with: the
// version the control plane has recorded for the management cluster, fetched
// from the control plane rather than taken from this machine.
func t47BundleManifest(t *testing.T, ctx context.Context, env t47Env, clusterID string) string {
	t.Helper()
	client, err := api.New(env.controlPlane, api.WithToken(env.token))
	if err != nil {
		t.Fatal(err)
	}
	version := env.bundle
	if record, err := client.BundleRecord(ctx, clusterID); err == nil && record.BundleVersion != "" {
		version = record.BundleVersion
	}
	raw, err := client.BundleManifest(ctx, version)
	if err != nil {
		t.Fatalf("fetching bundle %s for the wait's deadline: %v", version, err)
	}
	path := t.TempDir() + "/platform-" + version + ".yaml"
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// t47WaitFor retries until check holds, and fails the test naming what it was
// waiting for and the last thing it saw. Every wait in this gate is bounded by
// something real — the bead's 60 s, the agent's report cadence — and a failure
// has to say which one ran out.
func t47WaitFor(t *testing.T, within, every time.Duration, what string, check func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		ok, detail := check()
		if ok {
			t.Logf("%s: %s", what, detail)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s; last observation: %s", within, what, detail)
		}
		time.Sleep(every)
	}
}

// t47After reports whether the second RFC3339 stamp is strictly older than the
// first. Instants, not strings: the runner publishes UTC with a Z and a
// comparison by text would break the moment a field gained an offset.
func t47After(left, right string) bool {
	l, lerr := time.Parse(time.RFC3339, left)
	r, rerr := time.Parse(time.RFC3339, right)
	if lerr != nil || rerr != nil {
		return false
	}
	return l.After(r)
}

// t47BackupVerdict reads the management cluster's `backup` check and asks judge
// what it says. The evidence envelope is handed to the judge as well, because
// two of these assertions are about the CONTROL PLANE's facts being in the
// verdict at all: a check whose detail carries no `control_plane_checkpoint` is
// a check the fold never reached, and a pass read off it would be vacuous.
func t47BackupVerdict(ctx context.Context, t *testing.T, cp t47API, clusterID string, judge func(t47Check, map[string]any) (bool, string)) (bool, string) {
	t.Helper()
	check := cp.BackupCheck(ctx, t, clusterID)
	if check == nil {
		return false, "the control plane has recorded no health assessment for the management cluster yet: no report has been ingested"
	}
	evidence, _ := check.Detail["control_plane_checkpoint"].(map[string]any)
	if evidence == nil {
		return false, fmt.Sprintf(
			"%s: the backup verdict carries no control_plane_checkpoint evidence, so the control plane's own recovery facts are not part of this cluster's verdict (the backend folds them only for the cluster its own control-plane kit record names)",
			check.ReasonCode)
	}
	return judge(*check, evidence)
}

// t47Unprotected is every security change the newest eligible checkpoint does
// not contain, as the verdict carries them.
func t47Unprotected(evidence map[string]any) []any {
	changes, _ := evidence["unprotected_security_changes"].([]any)
	return changes
}

// t47Suspend stops or resumes the checkpoint CronJob. Stopping the CronJob is
// the customer's own action, and it has to be visible: nothing takes a
// checkpoint while it is suspended.
func t47Suspend(ctx context.Context, t *testing.T, r k3s.Runner, suspended bool) {
	t.Helper()
	patch := `{"spec":{"suspend":false}}`
	if suspended {
		patch = `{"spec":{"suspend":true}}`
	}
	res, err := r.Run(ctx, "sudo -n k3s kubectl patch cronjob "+controlplane.CheckpointCronJobName+
		" -n "+controlplane.Namespace+" --type merge -p '"+patch+"'")
	if err != nil {
		t.Fatalf("patching the checkpoint CronJob: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("patching the checkpoint CronJob: exit %d: %s", res.ExitCode, res.Stderr)
	}
}

// t47StatusDocument reads the checkpoint status ConfigMap's document as a map,
// so the subtest below can put it back exactly as it found it.
func t47StatusDocument(ctx context.Context, r k3s.Runner) (map[string]any, error) {
	out, err := k3s.Kubectl(ctx, r, "get configmap "+controlplane.CheckpointStatusConfigMap+" -n "+controlplane.Namespace+" -o json")
	if err != nil {
		return nil, fmt.Errorf("reading the checkpoint status: %w", err)
	}
	var configMap struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &configMap); err != nil {
		return nil, err
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(configMap.Data["status"]), &document); err != nil {
		return nil, fmt.Errorf("the checkpoint status is not valid JSON: %w", err)
	}
	return document, nil
}

// t47RewindCheckpoint makes the newest eligible checkpoint older than the
// staleness threshold.
//
// THIS IS THE ONE COMPRESSION IN THIS GATE, and it is deliberate. The threshold
// is 48 hours, so "stopping the CronJob raises the warning" cannot be observed
// by stopping it and waiting: what a stopped CronJob produces is an AGE, and the
// age is a field. The CronJob is suspended before this so the next scheduled run
// cannot publish a fresh timestamp over it, and the subtest's cleanup puts the
// original document back.
func t47RewindCheckpoint(t *testing.T, document map[string]any, by time.Duration) {
	t.Helper()
	latest, ok := document["latest_eligible"].(map[string]any)
	if !ok {
		t.Fatal("the checkpoint status carries no latest_eligible: this gate needs a checkpoint from step (a)")
	}
	original, _ := latest["at"].(string)
	latest["at"] = time.Now().Add(-by).UTC().Format("2006-01-02T15:04:05Z")
	t.Logf("rewinding latest_eligible.at from %s to %s: 49 hours of a stopped CronJob, without waiting for them", original, latest["at"])
}

// t47WriteStatus writes a checkpoint status document back, over stdin: the
// shell on the host never sees the JSON in a command string.
func t47WriteStatus(ctx context.Context, r k3s.Runner, document map[string]any) error {
	raw, err := json.Marshal(document)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"data": map[string]any{"status": string(raw)}})
	if err != nil {
		return err
	}
	res, err := r.RunInput(ctx, "sudo -n k3s kubectl patch configmap "+controlplane.CheckpointStatusConfigMap+
		" -n "+controlplane.Namespace+" --type merge --patch-file /dev/stdin", bytes.NewReader(patch))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("patching the checkpoint status: exit %d: %s", res.ExitCode, res.Stderr)
	}
	return nil
}

// t47PodSpec is the slice of a pod template this gate compares between the
// CronJob the chart installed and the Job an on-demand run created from it.
type t47PodSpec struct {
	ServiceAccountName string `json:"serviceAccountName"`
	Containers         []struct {
		Name  string `json:"name"`
		Image string `json:"image"`
		Env   []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"env"`
	} `json:"containers"`
	Volumes []struct {
		Name string `json:"name"`
		PVC  *struct {
			ClaimName string `json:"claimName"`
		} `json:"persistentVolumeClaim"`
	} `json:"volumes"`
}

func (p t47PodSpec) image() string {
	if len(p.Containers) == 0 {
		return ""
	}
	return p.Containers[0].Image
}

func (p t47PodSpec) scratchClaim() string {
	for _, volume := range p.Volumes {
		if volume.Name == "scratch" && volume.PVC != nil {
			return volume.PVC.ClaimName
		}
	}
	return ""
}

func (p t47PodSpec) retentionSeconds() int64 {
	for _, container := range p.Containers {
		for _, env := range container.Env {
			if env.Name == "CHECKPOINT_RETENTION_SECONDS" {
				seconds, err := strconv.ParseInt(env.Value, 10, 64)
				if err != nil {
					return 0
				}
				return seconds
			}
		}
	}
	return 0
}

// t47CronJobDoc is the checkpoint CronJob the chart installed, as far as this
// gate reads it.
type t47CronJobDoc struct {
	Spec struct {
		Schedule    string `json:"schedule"`
		JobTemplate struct {
			Spec struct {
				TTLSecondsAfterFinished *int64 `json:"ttlSecondsAfterFinished"`
				Template                struct {
					Spec t47PodSpec `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		} `json:"jobTemplate"`
	} `json:"spec"`
}

func (c t47CronJobDoc) retentionSeconds() int64 {
	return c.Spec.JobTemplate.Spec.Template.Spec.retentionSeconds()
}

func (c t47CronJobDoc) image() string { return c.Spec.JobTemplate.Spec.Template.Spec.image() }

func (c t47CronJobDoc) scratchClaim() string {
	return c.Spec.JobTemplate.Spec.Template.Spec.scratchClaim()
}

func t47CronJob(ctx context.Context, r k3s.Runner) (t47CronJobDoc, error) {
	var doc t47CronJobDoc
	out, err := k3s.Kubectl(ctx, r, "get cronjob "+controlplane.CheckpointCronJobName+" -n "+controlplane.Namespace+" -o json")
	if err != nil {
		return doc, fmt.Errorf("reading the checkpoint CronJob the chart installed: %w", err)
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return doc, err
	}
	return doc, nil
}

// t47JobDoc is one Job object.
type t47JobDoc struct {
	Metadata struct {
		Name              string `json:"name"`
		CreationTimestamp string `json:"creationTimestamp"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Spec t47PodSpec `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

func (j t47JobDoc) image() string        { return j.Spec.Template.Spec.image() }
func (j t47JobDoc) scratchClaim() string { return j.Spec.Template.Spec.scratchClaim() }

// t47ManualJob is the newest Job this gate's on-demand run created: the Job the
// CLI builds FROM the CronJob is named with its own `manual` stamp, so it is
// distinguishable from the schedule's Jobs and the backend's security-triggered
// ones.
func t47ManualJob(ctx context.Context, r k3s.Runner) (t47JobDoc, error) {
	var doc t47JobDoc
	out, err := k3s.Kubectl(ctx, r, "get jobs -n "+controlplane.Namespace+" -o json")
	if err != nil {
		return doc, fmt.Errorf("listing the checkpoint Jobs: %w", err)
	}
	var list struct {
		Items []t47JobDoc `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return doc, err
	}
	prefix := controlplane.OnDemandJobPrefix()
	var newest *t47JobDoc
	for i := range list.Items {
		job := &list.Items[i]
		if !strings.HasPrefix(job.Metadata.Name, prefix) {
			continue
		}
		if newest == nil || job.Metadata.CreationTimestamp > newest.Metadata.CreationTimestamp {
			newest = job
		}
	}
	if newest == nil {
		return doc, fmt.Errorf("no Job named %s* exists: the on-demand run did not create one from the CronJob", prefix)
	}
	return *newest, nil
}

// t47API is the control-plane surface this gate needs beyond what pkg/api
// already speaks: the session login, the CLI-token mint and revoke, and the
// fleet-health read (there is no `kubenest health` command yet — kn-9pgx builds
// it). Everything is the same endpoints the CLI's own client uses.
type t47API struct {
	base  string
	token string
	http  *http.Client
}

func (a t47API) Orgs(ctx context.Context) ([]api.Org, error) {
	client, err := api.New(a.base, api.WithToken(a.token))
	if err != nil {
		return nil, err
	}
	return client.ListOrgs(ctx)
}

// Auth logs in as the control plane's administrator and returns the session
// token. Minting and revoking a CLI token take a user session: a CLI token
// cannot mint or revoke another, which is why the revocation below is a real
// security change rather than a self-service edit.
func (a t47API) Auth(ctx context.Context, t *testing.T, email, password string) string {
	t.Helper()
	form := url.Values{"username": {email}, "password": {password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/api/v1/login", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := a.send(req, &out); err != nil {
		t.Fatalf("logging in as %s: %v. Set KUBENEST_ADMIN_EMAIL and KUBENEST_ADMIN_PASSWORD to the account the control plane's install created", email, err)
	}
	if out.AccessToken == "" {
		t.Fatal("the control plane accepted the login and returned no access_token")
	}
	return out.AccessToken
}

// ManagementCluster reads the cluster the control plane itself runs in, from
// the control-plane recovery kit's record — the backend's own source for it.
func (a t47API) ManagementCluster(ctx context.Context, t *testing.T, session, orgID string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.base+"/api/v1/orgs/"+url.PathEscape(orgID)+"/recovery-kits/control-plane/latest", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+session)
	var out struct {
		ClusterID string `json:"cluster_id"`
	}
	if err := a.send(req, &out); err != nil {
		t.Fatalf("reading the control-plane recovery kit's record: %v", err)
	}
	if out.ClusterID == "" {
		t.Fatal("the control-plane recovery kit names no cluster: the install records the management cluster on that kit, and without it the backend folds the control plane's checkpoints into no cluster's verdict")
	}
	return out.ClusterID
}

// MintAndRevoke mints a CLI token and revokes it, and returns a description of
// the change for the failure messages.
func (a t47API) MintAndRevoke(ctx context.Context, t *testing.T, session, name string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"name": name, "scopes": []string{"clusters:read"}, "ttl_days": 1})
	if err != nil {
		t.Fatal(err)
	}
	mint, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/api/v1/user/me/cli-tokens", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	mint.Header.Set("Authorization", "Bearer "+session)
	mint.Header.Set("Content-Type", "application/json")
	var created struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := a.send(mint, &created); err != nil {
		t.Fatalf("minting the CLI token this gate revokes: %v", err)
	}
	if created.ID == "" || created.Token == "" {
		t.Fatalf("the control plane minted a CLI token and did not return it: %+v", created)
	}

	revoke, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		a.base+"/api/v1/user/me/cli-tokens/"+url.PathEscape(created.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	revoke.Header.Set("Authorization", "Bearer "+session)
	if err := a.send(revoke, nil); err != nil {
		t.Fatalf("revoking CLI token %s: %v", name, err)
	}
	return "CLI token " + name
}

// t47Check is one health check's verdict, as the fleet-health API states it.
type t47Check struct {
	Check      string         `json:"check"`
	Status     string         `json:"status"`
	ReasonCode string         `json:"reason_code"`
	Message    string         `json:"message"`
	Detail     map[string]any `json:"detail"`
}

// BackupCheck reads the management cluster's `backup` check. A nil check is
// "the backend has not assessed this cluster yet".
func (a t47API) BackupCheck(ctx context.Context, t *testing.T, clusterID string) *t47Check {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/api/v1/clusters/"+url.PathEscape(clusterID)+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	var out struct {
		Checks []t47Check `json:"checks"`
	}
	if err := a.send(req, &out); err != nil {
		t.Fatalf("reading the cluster's health: %v", err)
	}
	for i := range out.Checks {
		if out.Checks[i].Check == "backup" {
			return &out.Checks[i]
		}
	}
	return nil
}

// send performs one request and decodes its JSON body, if any.
func (a t47API) send(req *http.Request, out any) error {
	req.Header.Set("Accept", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s %s: unreadable JSON: %w", req.Method, req.URL.Path, err)
	}
	return nil
}
