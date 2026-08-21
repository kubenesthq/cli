package install_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/manifest"
)

func testBundle(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(`
bundle: "1.0"
core:
  traefik: 41.2.0
limits:
  timeouts:
    install-total: 30m
    component-ready: 10m
    node-ready: 5m
`))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func testOpts() install.Options {
	return install.Options{
		Bundle:  "1.0",
		Name:    "prod-1",
		Servers: []string{"10.0.1.10"},
		HATier:  "single-server",
	}
}

// recorder captures the wire events, which is what the console and the
// failure-injection gate actually read.
type recorder struct {
	events []install.Event
	err    error
}

func (r *recorder) Emit(_ context.Context, e install.Event) error {
	r.events = append(r.events, e)
	return r.err
}

func (r *recorder) statuses() []string {
	var out []string
	for _, e := range r.events {
		out = append(out, e.Stage+":"+string(e.Status))
	}
	return out
}

func newSession(t *testing.T, rec *recorder) *install.Session {
	t.Helper()
	opts := testOpts()
	path := filepath.Join(t.TempDir(), "journal.json")
	j, err := install.OpenJournal(path, opts.Identity())
	if err != nil {
		t.Fatal(err)
	}
	return &install.Session{
		RunID:   "run-1",
		Opts:    opts,
		Bundle:  testBundle(t),
		Journal: j,
		Emitter: rec,
		Out:     io_Discard{},
	}
}

type io_Discard struct{}

func (io_Discard) Write(p []byte) (int, error) { return len(p), nil }

// stages builds a table of no-op stages with the real names, so sequencing
// can be exercised without a host.
func stages(t *testing.T, ran *[]string, fail map[string]error) []install.Stage {
	t.Helper()
	var out []install.Stage
	for _, name := range install.StageNames {
		name := name
		out = append(out, install.Stage{
			Name:      name,
			AlwaysRun: name == install.StagePreflight || name == install.StageRegister || name == install.StageVerify,
			Run: func(ctx context.Context, s *install.Session) error {
				*ran = append(*ran, name)
				return fail[name]
			},
		})
	}
	return out
}

func TestExecuteRunsThirteenStagesInOrder(t *testing.T) {
	rec := &recorder{}
	s := newSession(t, rec)
	var ran []string

	res, err := install.Execute(context.Background(), s, stages(t, &ran, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(ran) != 13 {
		t.Fatalf("ran %d stages, want 13: %v", len(ran), ran)
	}
	for i, name := range install.StageNames {
		if ran[i] != name {
			t.Fatalf("stage %d is %q, want %q — the order is not arbitrary, every stage depends on the ones above it", i+1, ran[i], name)
		}
	}
	if len(res.Skipped) != 0 {
		t.Errorf("a first run skips nothing, skipped %v", res.Skipped)
	}
	// 13 stages x started+completed.
	if len(rec.events) != 26 {
		t.Errorf("emitted %d events, want 26 (started+completed per stage)", len(rec.events))
	}
	for _, e := range rec.events {
		if e.StageTotal != 13 {
			t.Errorf("event for %s carries StageTotal %d, want 13", e.Stage, e.StageTotal)
		}
		if e.RunID != "run-1" {
			t.Errorf("event for %s carries RunID %q, want the process's run id", e.Stage, e.RunID)
		}
		if e.BundleVersion != "1.0" {
			t.Errorf("event for %s carries bundle %q", e.Stage, e.BundleVersion)
		}
	}
}

// Stage 1 writes nothing and stage 2 writes only to the control plane: a
// failure before stage 3 must not tell the operator to uninstall machines
// that were never touched.
func TestPreflightFailureSaysNothingWasWritten(t *testing.T) {
	rec := &recorder{}
	s := newSession(t, rec)
	var ran []string

	_, err := install.Execute(context.Background(), s,
		stages(t, &ran, map[string]error{install.StagePreflight: errors.New("node 10.0.1.11: sudo -n true failed")}))
	if err == nil {
		t.Fatal("want an error")
	}
	var se *install.StageError
	if !errors.As(err, &se) {
		t.Fatalf("want a *StageError, got %T", err)
	}
	msg := err.Error()
	for _, want := range []string{"preflight", "sudo -n true failed", "Nothing was written to any node"} {
		if !strings.Contains(msg, want) {
			t.Errorf("preflight failure message is missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "uninstall") {
		t.Errorf("preflight failed before anything was written; telling the operator to uninstall is wrong:\n%s", msg)
	}
	if len(ran) != 1 {
		t.Errorf("a failed preflight must stop the install, ran %v", ran)
	}
}

// A failure after the first write reports which stage, which component, and
// exactly two exits.
func TestStageFailureNamesTheComponentAndBothExits(t *testing.T) {
	rec := &recorder{}
	s := newSession(t, rec)
	var ran []string
	table := stages(t, &ran, map[string]error{install.StageStorage: errors.New("pod openebs-lvm-node-x in openebs is CrashLoopBackOff — volume group kubenest-vg not found")})
	for i := range table {
		if table[i].Name == install.StageStorage {
			table[i].Component = "openebs-lvm-localpv"
		}
	}

	_, err := install.Execute(context.Background(), s, table)
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{
		"platform-storage",
		"openebs-lvm-localpv",
		"volume group kubenest-vg not found",
		"resume",
		"kubenest platform uninstall --confirm",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure message is missing %q:\n%s", want, msg)
		}
	}

	// The wire event carries the same three things.
	last := rec.events[len(rec.events)-1]
	if last.Status != install.StatusFailed {
		t.Fatalf("last event is %s, want failed", last.Status)
	}
	if last.Component != "openebs-lvm-localpv" {
		t.Errorf("failed event names component %q — this is the field the failure-injection gate reads", last.Component)
	}
	if last.ReasonCode != "PLATFORM_STORAGE_FAILED" {
		t.Errorf("failed event reason code is %q", last.ReasonCode)
	}
	if !strings.Contains(last.Message, "volume group kubenest-vg not found") {
		t.Errorf("failed event message must carry the observed state verbatim, got %q", last.Message)
	}
}

// Resume: completed stages are skipped, the three always-run stages are not,
// and the install picks up where it stopped.
func TestResumeSkipsCompletedStagesButNeverPreflightRegisterOrVerify(t *testing.T) {
	opts := testOpts()
	path := filepath.Join(t.TempDir(), "journal.json")

	// First run fails at storage (stage 7).
	first := &recorder{}
	j1, err := install.OpenJournal(path, opts.Identity())
	if err != nil {
		t.Fatal(err)
	}
	s1 := &install.Session{RunID: "run-1", Opts: opts, Bundle: testBundle(t), Journal: j1, Emitter: first, Out: io_Discard{}}
	var ran1 []string
	if _, err := install.Execute(context.Background(), s1,
		stages(t, &ran1, map[string]error{install.StageStorage: errors.New("boom")})); err == nil {
		t.Fatal("want the first run to fail")
	}
	if len(ran1) != 7 {
		t.Fatalf("first run ran %v, want stages 1..7", ran1)
	}

	// Second run: same command, same journal file.
	second := &recorder{}
	j2, err := install.OpenJournal(path, opts.Identity())
	if err != nil {
		t.Fatal(err)
	}
	s2 := &install.Session{RunID: "run-2", Opts: opts, Bundle: testBundle(t), Journal: j2, Emitter: second, Out: io_Discard{}}
	var ran2 []string
	res, err := install.Execute(context.Background(), s2, stages(t, &ran2, nil))
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		install.StagePreflight, // always: writes nothing, hosts may have drifted
		install.StageRegister,  // always: the mint is once-only
		install.StageStorage,   // where it failed
		install.StageBackup, install.StageDay2, install.StageAgent,
		install.StageProfiles, install.StageRecord,
		install.StageVerify, // always: a skipped verify is not an install
	}
	if strings.Join(ran2, ",") != strings.Join(want, ",") {
		t.Errorf("resume ran\n  %v\nwant\n  %v", ran2, want)
	}
	wantSkipped := []string{install.StageK3sServer, install.StageK3sAgents, install.StageNetworking, install.StageCerts}
	if strings.Join(res.Skipped, ",") != strings.Join(wantSkipped, ",") {
		t.Errorf("resume skipped %v, want %v", res.Skipped, wantSkipped)
	}

	// Skipped stages still emit both transitions, so a console watching a
	// resumed install shows thirteen stages, not nine.
	if got := len(second.events); got != 26 {
		t.Errorf("a resumed run emitted %d events, want 26 — the console must still see all thirteen stages", got)
	}
	for _, e := range second.events {
		if e.Stage == install.StageK3sServer && !strings.Contains(e.Message, "skipped") {
			t.Errorf("a skipped stage must say so: %+v", e)
		}
	}
}

// An emitter that cannot reach the control plane must not fail an install
// that is otherwise succeeding on the machines.
func TestEmitterFailureDoesNotFailTheInstall(t *testing.T) {
	rec := &recorder{err: errors.New("control plane unreachable")}
	s := newSession(t, rec)
	var ran []string
	if _, err := install.Execute(context.Background(), s, stages(t, &ran, nil)); err != nil {
		t.Fatalf("telemetry must not be able to fail the thing it observes: %v", err)
	}
	if len(ran) != 13 {
		t.Errorf("ran %d stages, want 13", len(ran))
	}
}

// A stage with no implementation is refused rather than reported as done.
func TestUnwiredStageIsRefused(t *testing.T) {
	rec := &recorder{}
	s := newSession(t, rec)
	_, err := install.Execute(context.Background(), s, []install.Stage{{Name: install.StagePreflight}})
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("want a refusal for an unwired stage, got %v", err)
	}
}

// The journal on disk is a record of the run, including where it stopped.
func TestJournalRecordsEveryTransition(t *testing.T) {
	rec := &recorder{}
	s := newSession(t, rec)
	var ran []string
	_, _ = install.Execute(context.Background(), s,
		stages(t, &ran, map[string]error{install.StageCerts: errors.New("cert-manager webhook never became Ready")}))

	data, err := os.ReadFile(s.Journal.Path())
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, want := range []string{`"stage": "preflight"`, `"status": "completed"`, `"stage": "platform-certs"`, `"status": "failed"`, "cert-manager webhook never became Ready", `"run_id": "run-1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("journal is missing %q:\n%s", want, body)
		}
	}
	if entry, ok := s.Journal.LastFailure(); !ok || entry.Stage != install.StageCerts {
		t.Errorf("LastFailure = %+v, want platform-certs", entry)
	}
}
