//go:build e2e

// T4.4's hardware arm (kn-drill-waits-silently-for-ineligible-backup-0ut6).
//
// MEASURED ON A REAL HOST 2026-09-21: after `backup set-target` and one
// `backup now`, `kubenest backup drill` waited in silence. The operator's
// refusal was correct — the newest backup completed before the drill's proof
// workload became ready, so restoring it would prove nothing — but it left the
// result ConfigMap untouched, and the CLI waits on that object, so a first-time
// user got the bundle's two-hour `restore-drill` deadline and then a failure.
//
// The unit tests pin the record and the CLI's reading of it. This arm is the
// part no fake client can show: the operator running in the cluster records the
// refusal on its own poll, and the real command settles on it in the same
// second instead of waiting.
//
// FORCING THE INELIGIBLE STATE WITHOUT RACING THE OPERATOR. The obvious fixture
// is the bead's own: delete the proof's ready-at stamp and wait for the runner
// to re-stamp it, so readyAt moves to now and every existing backup falls before
// it. That races the operator, and the race was measured rather than imagined —
// on 2026-09-25 the drill started 17 seconds after a backup completed, inside
// the runner's 30-second poll. Losing that race leaves a `passed` record for the
// run we are about to measure. So this arm pushes the stamp FORWARD instead, by
// the bundle's own `backup` deadline plus a minute, before taking the backup:
// the runner only stamps when the annotation is absent, so the drill's window
// starts after any backup this bundle can produce, no eligible state ever
// exists, and the state under test is deterministic. The arm puts the stamp back
// in a cleanup, because a lab cluster left with a future stamp declines every
// drill until a backup completes after it.
//
// WHAT A HARDWARE RUN NEEDS
//
//   - a lab cluster from `./scripts/ephemeral-env.sh up --profile host`, with the
//     operator running from the candidate chart and k3s on the node;
//   - a backup target configured on that cluster (`kubenest backup set-target`
//     with an S3-compatible bucket): without it there is no drill configuration
//     and this arm skips. The proof workload's PVC also needs a storage class
//     that can bind, or the drill path was never reachable to begin with;
//   - the bundle the cluster was installed with, or at least one whose
//     limits.timeouts match: KUBENEST_BUNDLE (default 1.0) selects it, and this
//     arm writes the CLI's own embedded manifest out as --bundle-manifest, so
//     every deadline here is the bundle's and never a number in this file;
//   - the control plane and its token: every `backup` command checks the control
//     plane's contract era, so the arm signs in first;
//   - KUBENEST_GATE_CLUSTER naming the lab cluster, as gateEnvironment already
//     documents.
//
// Run from the umbrella workspace:
//
//	source lab/hetzner/.lab-env.sh
//	export KUBENEST_CONTROL_PLANE=http://localhost:8000 KUBENEST_CLI_TOKEN=knp_...
//	cd kubenest-cli && go test -tags e2e -v -timeout 30m ./e2e/ -run TestBackupDrillDeclinesIneligibleBackupImmediately
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/bundles"
	"kubenest.io/cli/pkg/cmd"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/uninstall"
)

const (
	// drillDeclineVeleroNamespace is where Velero and the drill's own objects
	// live: the drill's result ConfigMap beside them is what the CLI waits on.
	drillDeclineVeleroNamespace = "velero"
	drillDeclineResultConfigMap = "kubenest-restore-drill-result"
	drillDeclineConfigMap       = "kubenest-restore-drill"
	drillDeclineSourceNamespace = "kubenest-restore-drill-source"
	drillDeclineProofConfigMap  = "kubenest-restore-proof"
	// drillDeclineReadyAtAnnotation is the stamp the runner sets when the proof
	// workload reported ready. The drill only restores a backup that COMPLETED
	// after it, so the stamp is what makes a backup eligible.
	drillDeclineReadyAtAnnotation = "kubenest.io/restore-drill-ready-at"

	// drillDeclineBound is this arm's pass limit, not the product's deadline:
	// the defect was a two-hour wait, so a drill that takes longer than a minute
	// has not fixed it.
	drillDeclineBound = 60 * time.Second
)

// TestBackupDrillDeclinesIneligibleBackupImmediately is the planted negative's
// hardware half. With the operator's decline record reverted (the old silent
// `return nil` in the reconcile branch) the CLI never sees its request token
// answered, so `backup drill` runs into the context deadline at
// drillDeclineBound and fails on both the elapsed and the message assertions.
func TestBackupDrillDeclinesIneligibleBackupImmediately(t *testing.T) {
	env := gateEnvironment(t)
	ctx := context.Background()
	bundlePath := drillDeclineBundleManifest(t, env)
	bundle, err := manifest.Load(bundlePath)
	if err != nil {
		t.Fatalf("reading the bundle manifest this arm bounds its waits with: %v", err)
	}
	backupDeadline, err := bundle.Limits.Timeouts.For("backup")
	if err != nil {
		t.Fatal(err)
	}
	componentReady, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		t.Fatal(err)
	}

	nodes := connectNodes(t, env)
	if !drillDeclineConfigured(t, nodes) {
		t.Skip("this cluster has no backup target: velero/" + drillDeclineConfigMap +
			" is absent, so the operator is not running the drill path at all. Run `kubenest backup set-target` on the lab cluster, then this arm")
	}
	// The command checks the control plane's contract era before doing any
	// backup work, and this arm runs under its own HOME like every other e2e
	// gate, so the token has to be signed in first.
	t.Setenv("HOME", t.TempDir())
	gateLogin(t, env)

	// The drill's window opens after any backup this bundle can produce, and it
	// is open before the backup below is taken.
	window := drillDeclineOpenTheWindow(t, ctx, nodes, backupDeadline, componentReady)
	drillDeclineRunCLI(t, env, bundlePath, "now")
	recordBefore := drillDeclineResultRecord(t, nodes)
	newest, completedAt := drillDeclineWaitForCompletedBackup(t, ctx, nodes, backupDeadline)
	t.Logf("the newest completed backup is %s, completed %s, window opened %s", newest, completedAt, window)
	completed, err := time.Parse(time.RFC3339, completedAt)
	if err != nil {
		t.Fatalf("Velero reported completion time %q, which is not RFC3339: %v", completedAt, err)
	}
	if !completed.Before(window) {
		t.Fatalf("the backup completed at %s, not before the drill window's %s: this arm cannot measure the refusal it is here for",
			completedAt, window.Format(time.RFC3339))
	}

	// The drill must settle on the operator's refusal, at once, and fail.
	var out bytes.Buffer
	deadlineCtx, cancel := context.WithTimeout(ctx, drillDeclineBound)
	defer cancel()
	started := time.Now()
	drillErr := drillDeclineRunCLIWithContext(deadlineCtx, &out, env, bundlePath, "drill")
	elapsed := time.Since(started)
	reported := out.String()
	if drillErr != nil {
		reported += "\n" + drillErr.Error()
	}

	// A non-zero exit is how the command reports this: its RunE returns the
	// error, so the process exits non-zero with the sentence below as the
	// operator's only output.
	if drillErr == nil {
		t.Fatalf("the drill reported success on a backup that cannot contain the proof data:\n%s", reported)
	}
	if elapsed > drillDeclineBound {
		t.Errorf("the declined drill took %s, over this arm's %s limit: it waited on a deadline instead of"+
			" reading the operator's refusal\n%s", elapsed.Round(time.Second), drillDeclineBound, reported)
	}
	for _, want := range []string{newest, "kubenest backup now"} {
		if !strings.Contains(reported, want) {
			t.Errorf("the declined drill does not tell the operator %q:\n%s", want, reported)
		}
	}
	if strings.Contains(reported, "restore drill passed") {
		t.Errorf("a declined drill printed a pass line:\n%s", reported)
	}

	// The evidence record is NOT the drill's to write on a refusal. Compared
	// byte for byte rather than asserted to be never_run: a shared lab cluster's
	// own schedule may legitimately have passed a drill earlier, and what must
	// never happen is that this refused request changes what the upgrade gate,
	// fleet health and the console read.
	if after := drillDeclineResultRecord(t, nodes); after != recordBefore {
		t.Errorf("the refused request rewrote the drill evidence record:\nbefore: %s\nafter:  %s", recordBefore, after)
	} else if record := strings.TrimSpace(recordBefore); record == `{"status":"never_run"}` {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(record), &parsed); err != nil {
			t.Errorf("the never-run record is not JSON: %v\n%s", err, record)
		}
		for _, terminal := range []string{"backup", "completed_at", "duration_seconds", "verification", "failure"} {
			if _, present := parsed[terminal]; present {
				t.Errorf("the never-run record carries terminal field %s: %v", terminal, parsed)
			}
		}
	}
}

// drillDeclineBundleManifest writes the CLI's own embedded bundle manifest out
// as a file, because --bundle-manifest takes a path and this arm must bound its
// waits with the bundle's numbers rather than one written here.
func drillDeclineBundleManifest(t *testing.T, env gateEnv) string {
	t.Helper()
	raw, err := bundles.Raw(env.bundle)
	if err != nil {
		t.Fatalf("reading bundle %s from the CLI's catalog: %v", env.bundle, err)
	}
	path := filepath.Join(t.TempDir(), "platform-"+env.bundle+".yaml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("writing bundle %s out for --bundle-manifest: %v", env.bundle, err)
	}
	return path
}

// drillDeclineConfigured reports whether the cluster has a drill to observe at
// all. An unconfigured target is the documented `backup: unconfigured` state,
// not a failure of this arm.
func drillDeclineConfigured(t *testing.T, nodes []uninstall.Node) bool {
	t.Helper()
	out, err := k3s.Kubectl(context.Background(), nodes[0].Runner,
		"get configmap "+drillDeclineConfigMap+" -n "+drillDeclineVeleroNamespace+" -o name")
	if err != nil {
		t.Logf("no drill configuration: %v", err)
		return false
	}
	return strings.Contains(out, drillDeclineConfigMap)
}

// drillDeclineOpenTheWindow writes a ready-at stamp after any backup this
// bundle can produce and returns it. The cleanups put the proof back to the
// state a real cluster is in: without a stamp, the runner re-stamps it with the
// moment the proof became ready, which is the truth about the workload.
func drillDeclineOpenTheWindow(
	t *testing.T, ctx context.Context, nodes []uninstall.Node, backupDeadline, componentReady time.Duration,
) time.Time {
	t.Helper()
	window := time.Now().UTC().Add(backupDeadline + time.Minute)
	drillDeclineKubectl(t, nodes,
		"patch configmap "+drillDeclineProofConfigMap+" -n "+drillDeclineSourceNamespace+
			" --type=merge -p '{\"metadata\":{\"annotations\":{\""+drillDeclineReadyAtAnnotation+
			"\":\""+window.Format(time.RFC3339)+"\"}}}'")
	t.Cleanup(func() {
		out, err := k3s.Kubectl(context.Background(), nodes[0].Runner,
			"patch configmap "+drillDeclineProofConfigMap+" -n "+drillDeclineSourceNamespace+
				" --type=json -p '[{\"op\":\"remove\",\"path\":\"/metadata/annotations/"+
				strings.ReplaceAll(drillDeclineReadyAtAnnotation, "/", "~1")+"\"}]'")
		if err != nil {
			t.Logf("cleanup: the proof's ready-at stamp is still set to %s; a drill on this cluster declines until a backup completes after it: %s",
				window.Format(time.RFC3339), out)
		}
	})

	// Read it back before using it. The operator re-reads this annotation on
	// every pass, so a stamp that did not land is a stamp that changes nothing.
	_, err := converge.Wait(ctx, func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, nodes[0].Runner,
			"get configmap "+drillDeclineProofConfigMap+" -n "+drillDeclineSourceNamespace+
				" -o jsonpath='{.metadata.annotations.kubenest\\.io/restore-drill-ready-at}'")
		if err != nil {
			return false, converge.State{Object: "restore proof stamp", Status: "unreadable"}, err
		}
		got := strings.TrimSpace(out)
		if got != window.Format(time.RFC3339) {
			return false, converge.State{Object: "restore proof stamp", Status: got}, nil
		}
		return true, converge.State{Object: "restore proof stamp", Status: got}, nil
	}, converge.Options{
		Name:     "restore-proof-window",
		Deadline: componentReady,
		Reporter: converge.NewTextReporter(io.Discard),
	})
	if err != nil {
		t.Fatalf("waiting for the drill window's stamp to read back as %s: %v", window.Format(time.RFC3339), err)
	}
	return window
}

func drillDeclineRunCLI(t *testing.T, env gateEnv, bundlePath string, verb string, extra ...string) {
	t.Helper()
	var out bytes.Buffer
	if err := drillDeclineRunCLIWithContext(context.Background(), &out, env, bundlePath, verb, extra...); err != nil {
		t.Fatalf("kubenest backup %s: %v\n%s", verb, err, out.String())
	}
	t.Logf("kubenest backup %s: %s", verb, strings.TrimSpace(out.String()))
}

func drillDeclineRunCLIWithContext(ctx context.Context, out io.Writer, env gateEnv, bundlePath, verb string, extra ...string) error {
	args := []string{"backup", verb,
		"--cluster", env.cluster,
		"--server", env.server,
		"--bundle-manifest", bundlePath,
	}
	if env.sshUser != "" {
		args = append(args, "--ssh-user", env.sshUser)
	}
	if env.sshKey != "" {
		args = append(args, "--ssh-key", env.sshKey)
	}
	return drillDeclineRunCLIWithStdinContext(ctx, out, nil, append(args, extra...)...)
}

// drillDeclineRunCLIWithStdinContext runs the real command tree, so what this
// arm asserts is the operator's output and exit, not a library call's return
// value.
func drillDeclineRunCLIWithStdinContext(ctx context.Context, out io.Writer, in io.Reader, args ...string) error {
	root := cmd.NewRootCommand()
	root.SetOut(out)
	root.SetErr(out)
	if in != nil {
		root.SetIn(in)
	}
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}

func drillDeclineKubectl(t *testing.T, nodes []uninstall.Node, args string) string {
	t.Helper()
	out, err := k3s.Kubectl(context.Background(), nodes[0].Runner, args)
	if err != nil {
		t.Fatalf("kubectl %s: %v", args, err)
	}
	return strings.TrimSpace(out)
}

// drillDeclineResultRecord is the evidence record as its consumers read it.
func drillDeclineResultRecord(t *testing.T, nodes []uninstall.Node) string {
	t.Helper()
	return drillDeclineKubectl(t, nodes,
		"get configmap "+drillDeclineResultConfigMap+" -n "+drillDeclineVeleroNamespace+
			` -o jsonpath='{.data.result\.json}'`)
}

// drillDeclineVeleroBackup is the slice of `kubectl get backups -o json` this
// arm reads.
type drillDeclineVeleroBackup struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Phase               string `json:"phase"`
		CompletionTimestamp string `json:"completionTimestamp"`
	} `json:"status"`
}

// drillDeclineWaitForCompletedBackup waits until Velero reports a completed
// backup and returns the newest one — the backup the declined drill must name
// back to the operator — with its completion time. RFC3339 timestamps in UTC
// order correctly as strings, so the newest is the largest.
func drillDeclineWaitForCompletedBackup(
	t *testing.T, ctx context.Context, nodes []uninstall.Node, deadline time.Duration,
) (name, completedAt string) {
	t.Helper()
	_, err := converge.Wait(ctx, func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, nodes[0].Runner,
			"get backups -n "+drillDeclineVeleroNamespace+" -o json")
		if err != nil {
			return false, converge.State{Object: "velero backups", Status: "unreadable"}, err
		}
		var list struct {
			Items []drillDeclineVeleroBackup `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			return false, converge.State{Object: "velero backups", Status: "unparsable"}, err
		}
		name, completedAt = "", ""
		for _, item := range list.Items {
			if item.Status.Phase != "Completed" || item.Status.CompletionTimestamp < completedAt {
				continue
			}
			name, completedAt = item.Metadata.Name, item.Status.CompletionTimestamp
		}
		if name == "" {
			return false, converge.State{Object: "velero backups", Status: "no completed backup yet"}, nil
		}
		return true, converge.State{Object: "velero backup " + name, Status: "Completed at " + completedAt}, nil
	}, converge.Options{
		Name:     "completed-backup",
		Deadline: deadline,
		Reporter: converge.NewTextReporter(io.Discard),
	})
	if err != nil {
		t.Fatalf("waiting for a completed backup: %v", err)
	}
	return name, completedAt
}
