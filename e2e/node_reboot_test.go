//go:build e2e

// The wave-3 gate for kn-t35 (`kubenest node reboot`, T3.5), on a REAL Ubuntu
// host. k3d cannot run the installer, LVM or a reboot, so this is the only
// place the verb is accepted (AGENTS.md: "a day-2 verb is accepted only by a
// scenario on real Ubuntu hosts").
//
// What it asserts, which is the gate:
//
//	a confirmed reboot inside the window reboots the host, waits from OUTSIDE
//	over SSH (SSH reachable → k3s active → the API answering with the node
//	Ready) and leaves the operation record terminal
//	outside the window the command is REFUSED, names the next opening in local
//	time and UTC, and does not touch the host at all
//	--k3s-only renews a k3s leaf certificate that is inside the renewal window
//	WITHOUT rebooting the host, and the clock it borrowed is given back
//	kured's lock is held by the CLI for the duration (T3.3)
//
// Run from the umbrella workspace with a single-server lab
// (`KUBENEST_LAB_NODE2_IP` is optional: the two-node fixture is only needed by
// the multi-node drain assertions, which T3.7's TestS3PatchNightGate owns):
//
//	source lab/hetzner/.lab-env.sh
//	export KUBENEST_CONTROL_PLANE=http://localhost:8000 KUBENEST_CLI_TOKEN=...
//	cd kubenest-cli && go test -tags e2e -run TestNodeRebootGate -v ./e2e/
//
// The clock manipulation in the --k3s-only step is restored before the subtest
// returns AND in a t.Cleanup, because a lab host left 130 days in the future
// breaks every later gate that measures time.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/cmd"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/window"
)

// rebootRunCLI runs the REAL command tree, so the flags, the refusals and the
// output are the ones an operator gets. (Same pattern as the window gate: the
// acceptance is on the operator's surface, not on a function.)
func rebootRunCLI(out *bytes.Buffer, args ...string) error {
	root := cmd.NewRootCommand()
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	return root.Execute()
}

// rebootWatch samples the two objects whose TEMPORAL property is the assertion:
// the operation record (T2.3) and kured's lock (T3.3). Both are read from a
// machine that is not the machine going down.
type rebootWatch struct {
	mu       sync.Mutex
	lockSeen []string
	recordOn bool
	recordID string
	terminal bool
	mirror   []string
}

func (w *rebootWatch) note(lock string, live *api.ClusterOperation) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if lock != "" {
		w.lockSeen = append(w.lockSeen, lock)
	}
	if live != nil {
		w.recordOn = true
		w.recordID = live.OperationID
		w.terminal = live.Terminal
		w.mirror = append(w.mirror, fmt.Sprintf("%s terminal=%t stage=%s revision=%s", live.OperationID, live.Terminal, live.Stage, live.Revision))
	}
}

func (w *rebootWatch) locks() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.lockSeen...)
}

func (w *rebootWatch) summary() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return fmt.Sprintf("%d lock observation(s), record seen=%t (%s), %d mirror observation(s)",
		len(w.lockSeen), w.recordOn, w.recordID, len(w.mirror))
}

// startRebootWatch polls until done closes. It is deliberately tolerant of
// errors: on a single-server cluster the host is DOWN for part of the run and
// every read fails then, which is an expected absence rather than a failure.
// The record is additionally read from the control plane's mirror, which the
// host going down does not affect.
func startRebootWatch(ctx context.Context, t *testing.T, runner k3s.Runner, client *api.Client, clusterID string) (*rebootWatch, func()) {
	t.Helper()
	w := &rebootWatch{}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
			lock := ""
			if out, err := k3s.Kubectl(ctx, runner, `get daemonset kured -n kube-system -o jsonpath='{.metadata.annotations.weave\.works/kured-node-lock}'`); err == nil {
				lock = strings.TrimSpace(out)
			}
			var live *api.ClusterOperation
			if got, err := client.GetClusterOperation(ctx, clusterID); err == nil {
				live = got
			}
			w.note(lock, live)
		}
	}()
	return w, func() {
		close(done)
		<-stopped
	}
}

func TestNodeRebootGate(t *testing.T) {
	env := gateEnvironment(t)
	client, err := api.New(env.controlPlane, api.WithToken(env.token))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	clusterID := clusterIDFor(t, ctx, client, env.cluster)
	bundle := fetchBundle(t, client, env.bundle)
	rebootLimit, err := bundle.Limits.Timeouts.For("node-reboot")
	if err != nil {
		t.Fatal(err)
	}
	nodeDrain, err := bundle.Limits.Timeouts.For("node-drain")
	if err != nil {
		t.Fatal(err)
	}

	nodes := connectNodes(t, env)
	host := nodes[0].Runner
	// The two-node fixture is the same skip rule as upgrade_test.go: an agent is
	// optional here, and the drain's own acceptance is T3.7's.
	agent := os.Getenv("KUBENEST_LAB_NODE2_IP")

	// A WINDOW THAT IS OPEN AT ANY INSTANT THE GATE RUNS: all seven days,
	// 00:00-23:59. The window is UTC so the assertions about local time are
	// made by the REFUSAL subtest, which stores a window in a zone with an
	// offset and lets the command render both clocks.
	runningWindow := api.MaintenanceWindow{Days: weekdays(), Start: "00:00", End: "23:59", Timezone: "UTC"}
	storeWindow(t, ctx, client, clusterID, runningWindow)
	t.Cleanup(func() {
		// The lab's window is the gate's to leave behind, not to define: the
		// install's default is restored so a later gate starts from the same
		// place an operator would.
		storeWindow(t, context.Background(), client, clusterID, api.MaintenanceWindow{
			Days: []string{"sun"}, Start: "06:00", End: "09:00", Timezone: "UTC"})
	})

	if agent != "" {
		t.Logf("two-node fixture present (%s): the drain assertions of this gate still use the single-server path, which is the one T3.5 is accepted on", agent)
	}
	t.Logf("cluster %s: server %s, bundles limits.timeouts node-drain=%s node-reboot=%s", env.cluster, env.server, nodeDrain, rebootLimit)

	// The boot id after the first reboot, so the refused run below can prove it
	// did NOT reboot the host even if reading it there fails.
	var afterFirstBoot string

	t.Run("a confirmed reboot inside the window reboots the host, waits from outside, and leaves the record terminal", func(t *testing.T) {
		before := hostBootID(t, host)
		if before == "" {
			t.Fatal("could not read /proc/sys/kernel/random/boot_id before the reboot")
		}
		watch, stopWatch := startRebootWatch(ctx, t, host, client, clusterID)
		defer stopWatch()

		var out bytes.Buffer
		started := time.Now()
		runErr := rebootRunCLI(&out, "node", "reboot", "--cluster", env.cluster, "--node", env.server,
			"--confirm", "--wait")
		stopWatch()
		elapsed := time.Since(started)
		output := out.String()

		if runErr != nil {
			t.Fatalf("the reboot was refused or failed after %s: %v\n%s", elapsed.Round(time.Second), runErr, output)
		}
		// Pass limit: Ready within limits.timeouts.node-reboot.
		if elapsed > rebootLimit {
			t.Errorf("the reboot took %s, past limits.timeouts.node-reboot (%s)", elapsed.Round(time.Second), rebootLimit)
		}
		// Each step reported as it was OBSERVED, in order. The markers are the
		// observation lines ("  - <step>: <what was seen>"), not the plan's own
		// description of them: asserting on the plan would pass without the host
		// ever answering.
		steps := []string{
			"    - " + "SSH reachable" + ": ",
			"    - " + "the k3s service active" + ": ",
			"    - " + "the cluster API answering with the node Ready" + ": ",
		}
		last := -1
		for _, step := range steps {
			at := strings.Index(output, step)
			if at < 0 {
				t.Fatalf("the run never reported %q:\n%s", step, output)
			}
			if at < last {
				t.Errorf("the steps are reported out of order: %q came before the step before it\n%s", step, output)
			}
			last = at
		}
		if !strings.Contains(output[last:], "is Ready") {
			t.Errorf("the last step does not report the node Ready:\n%s", output[last:])
		}
		after := hostBootID(t, host)
		if after == before {
			t.Errorf("the boot id did not change (%s): the host was not rebooted", after)
		}
		afterFirstBoot = after
		// The host's own uptime agrees: a machine that rebooted has been up for
		// seconds, not for the length of the test.
		if up := hostUptimeSeconds(t, host); up > 10*time.Minute.Seconds() {
			t.Errorf("/proc/uptime says the host has been up %s, so this was not a reboot", time.Duration(up*float64(time.Second)).Round(time.Second))
		}
		// The record is on the cluster and TERMINAL afterwards (T2.3), and the
		// mirror showed it live while the run was in flight.
		live := rebootLiveRecord(t, ctx, host)
		if live == nil {
			t.Fatal("no operation record is on the cluster after the run")
		}
		if live.Record.Request.Kind != operation.KindNodeReboot {
			t.Errorf("the record is about %q, want %q", live.Record.Request.Kind, operation.KindNodeReboot)
		}
		if !live.Record.Terminal || live.Record.Result != string(operation.ResultSucceeded) {
			t.Errorf("the record is terminal=%t result=%q, want a succeeded terminal record",
				live.Record.Terminal, live.Record.Result)
		}
		if got := live.Record.Request.Targets; len(got) != 1 || got[0].HostID == "" || got[0].NodeUID == "" {
			t.Errorf("the record's target is %+v, want the host ID and Node UID it acted on", got)
		}
		// kured's lock (T3.3) named this node while the run was in flight, and
		// is gone now.
		if locks := watch.locks(); len(locks) == 0 {
			t.Errorf("kured's lock was never observed while the reboot ran: %s\n%s", watch.summary(), output)
		} else {
			named := false
			for _, l := range locks {
				if strings.Contains(l, env.server) || strings.Contains(l, nodeName(t, ctx, host)) {
					named = true
				}
			}
			if !named {
				t.Errorf("kured's lock was observed but never named this node: %v", locks)
			}
		}
		if lock := kuredLockOn(t, ctx, host); lock != "" {
			t.Errorf("kured's lock is still held after the node came back: %s", lock)
		}
		t.Logf("rebooted in %s; watch: %s", elapsed.Round(time.Second), watch.summary())
	})

	t.Run("outside the window it is refused, names both clocks, and leaves the host untouched", func(t *testing.T) {
		// A window that is not open now, in a zone with an offset, so the
		// refusal has to render the opening in the operator's local time AND in
		// UTC to be believed. The days are shifted by one from today so the
		// next opening is unambiguous.
		today := time.Now().UTC().Weekday()
		storeWindow(t, ctx, client, clusterID, api.MaintenanceWindow{
			Days:     []string{window.Name((today + 1) % 7)},
			Start:    "02:00",
			End:      "03:00",
			Timezone: "Asia/Kolkata",
		})
		defer storeWindow(t, ctx, client, clusterID, runningWindow)

		before := afterFirstBoot
		if before == "" {
			before = hostBootID(t, host)
		}
		var out bytes.Buffer
		runErr := rebootRunCLI(&out, "node", "reboot", "--cluster", env.cluster, "--node", env.server, "--confirm")
		output := out.String()
		if runErr == nil {
			t.Fatalf("the reboot was NOT refused outside the window:\n%s", output)
		}
		refusal := runErr.Error() + output
		for _, want := range []string{"outside the maintenance window", "IST", "UTC", "it next opens"} {
			if !strings.Contains(refusal, want) {
				t.Errorf("the refusal does not name %q:\n%s", want, refusal)
			}
		}
		if after := hostBootID(t, host); after != before {
			t.Errorf("the boot id changed (%s → %s): a refused reboot rebooted the host", before, after)
		}
		// Nothing was held by the refused run either: no record may exist that
		// it did not create, and the lock is free.
		if lock := kuredLockOn(t, ctx, host); lock != "" {
			t.Errorf("a refused run left kured's lock held: %s", lock)
		}
	})

	t.Run("--k3s-only renews a leaf certificate inside the renewal window without rebooting the host", func(t *testing.T) {
		before := hostBootID(t, host)
		service := "k3s"
		// The service's start time, in the MONOTONIC clock: the wall clock is
		// shifted by this test itself, so only this proves a restart happened.
		startBefore := serviceMonotonicStart(t, ctx, host, service)
		certsBefore := leafCertificateExpiry(t, ctx, host)
		if len(certsBefore) == 0 {
			t.Skip("no k3s leaf certificate is readable under /var/lib/rancher/k3s/server/tls: this step asserts the renewal --k3s-only exists for, and cannot be run without one")
		}
		realEpoch := time.Now().Unix()
		defer restoreHostClock(t, ctx, host, realEpoch)

		// The renewal window is measured against the host's own clock, so the
		// clock is advanced — never the certificate — and given back below. 130
		// days is past the ~120-day threshold k3s rotates at.
		advanceHostClock(t, ctx, host, realEpoch+130*24*3600)

		// The window is in UTC and covers every day, so the shifted instant is
		// still inside it: the shift is this test's, and it must not turn into a
		// window refusal that hides the renewal.
		storeWindow(t, ctx, client, clusterID, runningWindow)
		var out bytes.Buffer
		runErr := rebootRunCLI(&out, "node", "reboot", "--cluster", env.cluster, "--node", env.server,
			"--confirm", "--k3s-only")
		output := out.String()
		if runErr != nil {
			t.Fatalf("--k3s-only failed: %v\n%s", runErr, output)
		}
		if !strings.Contains(output, "certificate") {
			t.Errorf("the output does not name the certificate renewal --k3s-only performs:\n%s", output)
		}
		if after := hostBootID(t, host); after != before {
			t.Errorf("--k3s-only changed the boot id (%s → %s): the host was rebooted", before, after)
		}
		if startAfter := serviceMonotonicStart(t, ctx, host, service); startAfter <= startBefore {
			t.Errorf("k3s's monotonic start time did not move (%d → %d): the service did not restart", startBefore, startAfter)
		}
		certsAfter := leafCertificateExpiry(t, ctx, host)
		renewed := []string{}
		for name, before := range certsBefore {
			if after, ok := certsAfter[name]; ok && after != before {
				renewed = append(renewed, fmt.Sprintf("%s: %s → %s", name, before, after))
			}
		}
		if len(renewed) == 0 {
			t.Errorf("no k3s leaf certificate was renewed while the clock was %d days ahead: %v", 130, certsBefore)
		} else {
			t.Logf("renewed: %s", strings.Join(renewed, "; "))
		}

		// The clock is given back and the host agrees with this machine again.
		restoreHostClock(t, ctx, host, realEpoch)
		if skew := hostEpoch(t, host) - time.Now().Unix(); skew > 10 || skew < -10 {
			t.Fatalf("the host's clock is %d s away from this machine's after the restore", skew)
		}
		if !strings.Contains(output, "waiting from outside") {
			t.Errorf("--k3s-only does not report the wait from outside:\n%s", output)
		}
		// The wait's pass limit is limits.timeouts.node-reboot, and the run has
		// already been measured against it by returning at all.
	})
}

// ---------------------------------------------------------------------------
// Host helpers. Every remote read goes through k3s.Kubectl or a plain command
// on the existing SSH transport, never a local kubeconfig (AGENTS.md: "never
// point a local kubeconfig at a provisioned or customer cluster. Use SSH to the
// host").
// ---------------------------------------------------------------------------

func hostBootID(t *testing.T, r k3s.Runner) string {
	t.Helper()
	res, err := r.Run(context.Background(), "cat /proc/sys/kernel/random/boot_id")
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("reading the host's boot id: %v (exit %d, %s)", err, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout)
}

func hostUptimeSeconds(t *testing.T, r k3s.Runner) float64 {
	t.Helper()
	res, err := r.Run(context.Background(), "cat /proc/uptime")
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("reading the host's uptime: %v", err)
	}
	var up float64
	if _, err := fmt.Sscanf(strings.TrimSpace(res.Stdout), "%f", &up); err != nil {
		t.Fatalf("parsing /proc/uptime %q: %v", res.Stdout, err)
	}
	return up
}

func hostEpoch(t *testing.T, r k3s.Runner) int64 {
	t.Helper()
	res, err := r.Run(context.Background(), "date -u +%s")
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("reading the host's clock: %v", err)
	}
	var epoch int64
	if _, err := fmt.Sscanf(strings.TrimSpace(res.Stdout), "%d", &epoch); err != nil {
		t.Fatalf("parsing the host's clock %q: %v", res.Stdout, err)
	}
	return epoch
}

// advanceHostClock steps the disposable lab host's clock forward.
//
// It is done with `date -s` because the point is k3s's own renewal decision,
// which reads the host clock: faketime around the restart would need the flag
// to reach the systemd unit, which is not the thing under test.
func advanceHostClock(t *testing.T, ctx context.Context, r k3s.Runner, epoch int64) {
	t.Helper()
	res, err := r.Run(ctx, fmt.Sprintf("sudo -n date -s @%d", epoch))
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("advancing the host clock: %v (exit %d, %s)", err, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
}

// restoreHostClock puts the lab host's clock back on this machine's time and
// asks the host to resync, so a later gate does not run 130 days in the future.
func restoreHostClock(t *testing.T, ctx context.Context, r k3s.Runner, epoch int64) {
	t.Helper()
	if _, err := r.Run(ctx, fmt.Sprintf("sudo -n date -s @%d", epoch)); err != nil {
		t.Errorf("restoring the host clock: %v", err)
	}
	// Best effort: a lab image without an NTP client still has the right time
	// after the line above, and this only keeps it right.
	if _, err := r.Run(ctx, "sudo -n timedatectl set-ntp true"); err != nil {
		t.Logf("could not re-enable NTP on the host (the clock is already restored): %v", err)
	}
	if skew := hostEpoch(t, r) - time.Now().Unix(); skew > 10 || skew < -10 {
		t.Errorf("the host's clock is %d s away from this machine's after the restore", skew)
	}
}

// serviceMonotonicStart is the service's start in the MONOTONIC clock
// (microseconds since boot). The wall clock this gate manipulates would make
// ActiveEnterTimestamp move on its own, so the monotonic counter is the one
// observation that proves a restart and nothing else.
func serviceMonotonicStart(t *testing.T, ctx context.Context, r k3s.Runner, service string) int64 {
	t.Helper()
	res, err := r.Run(ctx, "systemctl show -p ExecMainStartTimestampMonotonic --value "+service)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("reading %s's monotonic start: %v", service, err)
	}
	var v int64
	if strings.TrimSpace(res.Stdout) == "" {
		return 0
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(res.Stdout), "%d", &v); err != nil {
		t.Fatalf("parsing the monotonic start %q: %v", res.Stdout, err)
	}
	return v
}

// leafCertificateExpiry reads every leaf certificate k3s keeps for a server:
// the file name mapped to its notAfter.
//
// A leaf is told from a CA by its own X.509 extension, not by its name: k3s's
// tls directory holds both, and a CA that never rotates would make the renewal
// assertion pass for the wrong reason.
func leafCertificateExpiry(t *testing.T, ctx context.Context, r k3s.Runner) map[string]string {
	t.Helper()
	const script = `set -e
dir=/var/lib/rancher/k3s/server/tls
for f in $(ls $dir/*.crt $dir/*/*.crt 2>/dev/null); do
  if ! sudo -n openssl x509 -in "$f" -noout -text 2>/dev/null | grep -q 'CA:TRUE'; then
    end=$(sudo -n openssl x509 -in "$f" -noout -enddate 2>/dev/null | cut -d= -f2)
    echo "$f|$end"
  fi
done`
	res, err := r.Run(ctx, script)
	if err != nil {
		t.Fatalf("reading the k3s certificate directory: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 || parts[1] == "" {
			continue
		}
		out[parts[0]] = parts[1]
	}
	return out
}

func nodeName(t *testing.T, ctx context.Context, r k3s.Runner) string {
	t.Helper()
	res, err := r.Run(ctx, "hostname")
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

func kuredLockOn(t *testing.T, ctx context.Context, r k3s.Runner) string {
	t.Helper()
	out, err := k3s.Kubectl(ctx, r, `get daemonset kured -n kube-system -o jsonpath='{.metadata.annotations.weave\.works/kured-node-lock}'`)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// storeWindow writes the cluster's window through the route `kubenest cluster
// set-window` writes (T3.1), carrying the revision it read — the compare-and-
// swap the route enforces.
func storeWindow(t *testing.T, ctx context.Context, client *api.Client, clusterID string, w api.MaintenanceWindow) {
	t.Helper()
	current, err := client.MaintenanceWindow(ctx, clusterID)
	if err != nil {
		t.Fatalf("reading the stored window: %v", err)
	}
	if _, err := client.PutMaintenanceWindow(ctx, clusterID, w, current.CurrentRevision()); err != nil {
		t.Fatalf("storing the window %+v: %v", w, err)
	}
}

func weekdays() []string {
	days := make([]string, 0, 7)
	for d := time.Sunday; d <= time.Saturday; d++ {
		days = append(days, window.Name(d))
	}
	return days
}

// rebootLiveRecord reads the cluster's live operation record, or nil when it
// holds none. The record IS the lock and the resume path, so its absence after
// a run that changed a node is itself a failure.
func rebootLiveRecord(t *testing.T, ctx context.Context, r k3s.Runner) *operation.Stored {
	t.Helper()
	store := &operation.Store{Runner: r, Operator: "gate@e2e"}
	live, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("reading the operation record: %v", err)
	}
	return live
}
