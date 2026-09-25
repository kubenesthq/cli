//go:build e2e

// T3.1's gate: the maintenance window, on a REAL cluster with a REAL control
// plane and the real clock.
//
// What this asserts, and why it cannot be asserted anywhere else:
//
//	a window the CLI stores is refused OUTSIDE, and the refusal names the next
//	  opening in BOTH local time and UTC, against a real clock in a real zone
//	`kubenest platform upgrade --wait` holds nothing while it waits, enters the
//	  window at the opening, TAKES THE OPERATION LOCK, and reports `inside`
//	  (local and UTC) before anything is changed
//
// Run it from the umbrella workspace with a lab cluster:
//
//	source lab/hetzner/.lab-env.sh
//	export KUBENEST_CONTROL_PLANE=http://localhost:8000 KUBENEST_CLI_TOKEN=...
//	cd kubenest-cli && go test -tags e2e -run TestMaintenanceWindowGate -v ./e2e/
//
// Pass limits, from the bead: the refusal inside 60 s of the run starting; the
// --wait run entering the window within 5 minutes of the opening.
//
// It needs the control-plane variables and the SSH key the other gates use,
// because gateEnvironment is what resolves the lab. The window it sets is left
// stored: a cluster's window is state an operator owns, there is no unset
// route, and the next run of this gate replaces it with its own window two
// minutes ahead.
package e2e

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/cmd"
	"kubenest.io/cli/pkg/config"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/window"
)

func TestMaintenanceWindowGate(t *testing.T) {
	env := gateEnvironment(t)
	ctx := context.Background()

	client, err := api.New(env.controlPlane, api.WithToken(env.token))
	if err != nil {
		t.Fatal(err)
	}
	clusterID := clusterIDFor(t, ctx, client, env.cluster)

	// The COMMAND is what is under test, so it runs against a HOME this test
	// owns: logged in to the lab's control plane, with its own journals.
	t.Setenv("HOME", t.TempDir())
	if err := config.Save(&config.Config{ControlPlaneURL: env.controlPlane}); err != nil {
		t.Fatal(err)
	}
	creds := &config.Credentials{Tokens: map[string]config.StoredCredential{}}
	creds.Set(env.controlPlane, env.token)
	if err := config.SaveCredentials(creds); err != nil {
		t.Fatal(err)
	}

	// One SSH connection to the server node: it is where the operation record
	// lives, so it is what tells "the wait held nothing" from "the wait held the
	// lock".
	runner := operationRunner(t, env)
	removeOperationRecords(t, runner)
	t.Cleanup(func() { removeOperationRecords(t, runner) })

	// A window that opens about two minutes from now, in a zone with a real
	// offset, so the refusal's local and UTC halves are two different renderings
	// of one instant rather than the same string twice.
	zone := envOr("KUBENEST_GATE_WINDOW_TZ", "Asia/Kolkata")
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatalf("KUBENEST_GATE_WINDOW_TZ=%q is not an IANA name: %v", zone, err)
	}
	t31StoreWindowOpeningSoon(t, ctx, client, clusterID, loc)

	// The opening comes from what the control plane STORED — canonicalized by
	// the backend — because that is the window the gate will compute from.
	stored, err := client.MaintenanceWindow(ctx, clusterID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Window == nil {
		t.Fatal("the control plane stored no window")
	}
	storedWindow, err := window.Parse(window.Spec{
		Days: stored.Window.Days, Start: stored.Window.Start, End: stored.Window.End, Timezone: stored.Window.Timezone,
	})
	if err != nil {
		t.Fatalf("the stored window is not one the CLI can represent: %v", err)
	}
	opening, ok := storedWindow.NextOpen(time.Now())
	if !ok {
		t.Fatalf("the stored window %s has no opening", storedWindow)
	}
	if left := time.Until(opening); left < 30*time.Second || left > 3*time.Minute {
		t.Fatalf("the window opens in %s, want about two minutes: this gate's pass limit is five", left.Round(time.Second))
	}
	local := opening.In(loc).Format("Mon 2 Jan 15:04 MST")
	utc := opening.UTC().Format("Mon 2 Jan 15:04 MST")
	if stored.State == "" {
		t.Fatal("the control plane reported no state for the stored window")
	}
	t.Logf("window %s stored at revision %d, state %s; it opens %s (%s)", storedWindow, stored.CurrentRevision(), stored.State, local, utc)

	current, err := client.BundleRecord(ctx, clusterID)
	if err != nil {
		t.Fatal(err)
	}
	to := t31NextBundle(t, ctx, client, current.BundleVersion)
	// The real command, with the real flags: this gate is accepted on the
	// operator's surface, not on a function in pkg/upgrade.
	cliArgs := func(target string, extra ...string) []string {
		args := []string{"platform", "upgrade", "--cluster", env.cluster, "--server", env.server,
			"--ssh-user", env.sshUser, "--to", target}
		if env.sshKey != "" {
			args = append(args, "--ssh-key", env.sshKey)
		}
		return append(args, extra...)
	}

	t.Run("outside the window kubenest platform upgrade is refused and names the next opening in local time and UTC", func(t *testing.T) {
		// Nothing is held by a refused run: no operation record may exist after
		// it, because it never reaches a stage that changes anything.
		if t31LiveRecord(t, ctx, runner) != nil {
			t.Fatal("an operation record exists before the refused run")
		}

		var out bytes.Buffer
		started := time.Now()
		err := t31RunCLI(&out, cliArgs(to)...)
		elapsed := time.Since(started)

		if err == nil {
			t.Fatalf("the upgrade was NOT refused: by the time the gates ran, the window had already opened (the lab took longer than the two minutes between storing the window and reaching preflight):\n%s", out.String())
		}
		if elapsed > 60*time.Second {
			t.Errorf("the refusal took %s, past this gate's 60 s limit", elapsed.Round(time.Second))
		}
		refusal := err.Error() + out.String()
		if !strings.Contains(refusal, "Maintenance window") {
			t.Errorf("the window gate did not refuse:\n%s", refusal)
		}
		if !strings.Contains(refusal, "outside") {
			t.Errorf("the refusal must say now is outside the window:\n%s", refusal)
		}
		for _, want := range []string{local, utc} {
			if !strings.Contains(refusal, want) {
				t.Errorf("the refusal does not name the next opening as %q; it must name it in local time AND UTC:\n%s", want, refusal)
			}
		}
		if !strings.Contains(refusal, "--wait") {
			t.Errorf("the refusal must name --wait as the way to hold for the window:\n%s", refusal)
		}
		if t31LiveRecord(t, ctx, runner) != nil {
			t.Error("a refused run left an operation record behind")
		}
	})

	t.Run("with --wait the run holds nothing, enters the window at the opening and takes the operation lock", func(t *testing.T) {
		if t31LiveRecord(t, ctx, runner) != nil {
			t.Fatal("an operation record exists before the --wait run")
		}
		if storedWindow.Contains(time.Now()) {
			t.Fatal("the window is already open: the --wait half of this gate cannot measure the wait")
		}

		// The target is the bundle the cluster ALREADY runs, so the run reaches
		// preflight inside the window and stops on the bundle-path gate. This
		// gate is the window's; starting a real bundle move here would be S2's
		// test (kn-1krv), not this one.
		out := &t31StampingWriter{}
		err := t31RunCLI(out, cliArgs(current.BundleVersion, "--wait")...)

		if out.enteredAt.IsZero() {
			t.Fatalf("the run never reported entering the window:\n%s", out.String())
		}
		if early := opening.Sub(out.enteredAt); early > 5*time.Second {
			t.Errorf("the run reported `inside` %s BEFORE the window opened", early.Round(time.Second))
		}
		if late := out.enteredAt.Sub(opening); late > 5*time.Minute {
			t.Errorf("the run entered the window %s after the opening, past this gate's 5 minute limit", late.Round(time.Second))
		}
		printed := out.String()
		if !strings.Contains(printed, "Waiting.") {
			t.Errorf("the run did not wait for the window:\n%s", printed)
		}
		if !strings.Contains(printed, "inside") || !strings.Contains(printed, storedWindow.String()) {
			t.Errorf("the run does not report being inside the window:\n%s", printed)
		}
		for _, want := range []string{local, utc} {
			if !strings.Contains(printed, want) {
				t.Errorf("`inside` must be reported in local time AND UTC, missing %q:\n%s", want, printed)
			}
		}

		// TAKES THE LOCK: the record exists after the run, names the upgrade and
		// is terminal — the run ended, the lock did not leak. It is read with
		// pkg/operation's own reader, not by matching text in kubectl's output:
		// the record is JSON nested inside a ConfigMap key.
		live := t31LiveRecord(t, ctx, runner)
		if live == nil {
			t.Fatal("the --wait run never took the operation lock")
		}
		if live.Record.Request.Kind != operation.KindUpgrade {
			t.Errorf("the record's kind is %q, want the upgrade kind: an upgrade record must not be adoptable by a restore", live.Record.Request.Kind)
		}
		if live.Record.Request.Cluster != env.cluster {
			t.Errorf("the record names cluster %q, want %q", live.Record.Request.Cluster, env.cluster)
		}
		if !live.Record.Terminal {
			t.Error("the run finished but left its operation record live, so the next operation would be refused by a record nothing is running")
		}

		// Every gate re-runs at the opening, and the window gate is one of the
		// ones that then passes: the run stopped on the transition it asked for,
		// not on the clock.
		if err == nil {
			t.Errorf("the run completed an upgrade to the bundle the cluster already runs:\n%s", printed)
		} else if strings.Contains(err.Error(), "Maintenance window") {
			t.Errorf("the window must be satisfied by the time the gates run: %v", err)
		}
	})
}

// t31StoreWindowOpeningSoon stores a window that opens about two minutes from
// now and lasts half an hour, and returns nothing: the caller reads the stored
// form back, because the control plane canonicalizes the day list and that is
// the window the gate will compute from.
func t31StoreWindowOpeningSoon(t *testing.T, ctx context.Context, client *api.Client, clusterID string, loc *time.Location) {
	t.Helper()
	// Seconds are truncated so the start is a whole minute: NextOpen returns the
	// minute boundary, and the refusal renders that instant.
	start := time.Now().Add(2 * time.Minute).In(loc).Truncate(time.Minute)
	end := start.Add(30 * time.Minute)

	current, err := client.MaintenanceWindow(ctx, clusterID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := client.PutMaintenanceWindow(ctx, clusterID, api.MaintenanceWindow{
		Days:     []string{window.Name(start.Weekday())},
		Start:    start.Format("15:04"),
		End:      end.Format("15:04"),
		Timezone: loc.String(),
	}, current.CurrentRevision())
	if err != nil {
		t.Fatalf("storing the window: %v", err)
	}
	if stored.Window == nil {
		t.Fatal("the write stored no window")
	}
	if stored.Revision == nil || *stored.Revision <= current.CurrentRevision() {
		t.Fatalf("the stored window is at revision %v, want one above the revision read (%d)", stored.Revision, current.CurrentRevision())
	}
	t.Logf("stored %s %s-%s %s at revision %d, state %s (was revision %d)",
		stored.Window.Days, stored.Window.Start, stored.Window.End, stored.Window.Timezone,
		*stored.Revision, stored.State, current.CurrentRevision())
}

// t31RunCLI runs the real command tree, so the flags, the config file and the
// refusals are the ones an operator gets.
func t31RunCLI(out io.Writer, args ...string) error {
	root := cmd.NewRootCommand()
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	return root.Execute()
}

// t31LiveRecord reads the cluster's live operation record, or nil when it holds
// none. The record IS the lock, so its absence is "nothing was held".
func t31LiveRecord(t *testing.T, ctx context.Context, runner k3s.Runner) *operation.Stored {
	t.Helper()
	store := &operation.Store{Runner: runner, Operator: "gate@e2e"}
	live, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("reading the operation record: %v", err)
	}
	return live
}

// t31StampingWriter records when the run first reported entering the window, so
// the 5 minute pass limit is measured against the output the run itself
// produced rather than against how long the whole upgrade took.
type t31StampingWriter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	enteredAt time.Time
}

func (w *t31StampingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	// The wait's closing line is the one that says every gate re-runs from
	// here; it is printed exactly once, after the loop has seen the window open.
	if w.enteredAt.IsZero() && strings.Contains(w.buf.String(), "Every gate re-runs from here") {
		w.enteredAt = time.Now()
	}
	return n, err
}

func (w *t31StampingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// t31NextBundle is the next bundle the catalog serves above the one the cluster
// runs. The version comes from the catalog, never invented.
func t31NextBundle(t *testing.T, ctx context.Context, client *api.Client, from string) string {
	t.Helper()
	bundles, err := client.ListBundles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fromMajor, fromMinor := t31Version(t, from)
	best, bestMajor, bestMinor := "", 0, 0
	for _, b := range bundles {
		major, minor := t31Version(t, b.Version)
		newer := major > fromMajor || (major == fromMajor && minor > fromMinor)
		if !newer {
			continue
		}
		if best == "" || major < bestMajor || (major == bestMajor && minor < bestMinor) {
			best, bestMajor, bestMinor = b.Version, major, minor
		}
	}
	if best == "" {
		t.Skipf("the catalog serves nothing above %s, so there is no forward transition to gate", from)
	}
	return best
}

func t31Version(t *testing.T, version string) (int, int) {
	t.Helper()
	major, rest, ok := strings.Cut(version, ".")
	if !ok {
		t.Fatalf("bundle version %q is not major.minor", version)
	}
	minor, err := strconv.Atoi(rest)
	if err != nil {
		t.Fatalf("bundle version %q is not major.minor: %v", version, err)
	}
	n, err := strconv.Atoi(major)
	if err != nil {
		t.Fatalf("bundle version %q is not major.minor: %v", version, err)
	}
	return n, minor
}
