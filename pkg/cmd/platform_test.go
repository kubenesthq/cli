package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/window"
)

func TestInstallFlagsValidate(t *testing.T) {
	valid := func() InstallFlags {
		return InstallFlags{
			Bundle:  "1.0",
			Name:    "prod-1",
			Servers: []string{"10.0.1.10"},
			HATier:  "single-server",
		}
	}

	if err := (&InstallFlags{}).Validate(); err == nil {
		t.Error("empty flags must not validate")
	}
	f := valid()
	if err := f.Validate(); err != nil {
		t.Errorf("valid single-server flags rejected: %v", err)
	}

	f = valid()
	f.HATier = "medium"
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "single-server") {
		t.Errorf("unknown HA tier must be rejected naming the real tiers, got %v", err)
	}

	f = valid()
	f.HATier = "ha"
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "three") {
		t.Errorf("ha with one server must demand three control-plane nodes, got %v", err)
	}
	f.Servers = []string{"a", "b", "c"}
	if err := f.Validate(); err != nil {
		t.Errorf("ha with three servers rejected: %v", err)
	}

	f = valid()
	f.Servers = []string{"a", "b"}
	if err := f.Validate(); err == nil {
		t.Error("single-server with two servers must be rejected")
	}
}

// The help example is a first action a reader can copy. It must show the
// command that builds a first cluster — the one that also installs the
// control plane — and the command that adds a later cluster to it. It must
// also not offer a component profile the current CLI cannot install.
func TestPlatformInstallHelpShowsTheFirstClusterPath(t *testing.T) {
	root := NewRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"platform", "install", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("platform install help: %v", err)
	}

	help := output.String()
	for _, want := range []string{
		// The first cluster: the control plane comes with it.
		// The bundle version on the next line is deliberately not asserted: it
		// moves with every release, and pinning it here only breaks this test
		// again on the next one.
		"kubenest platform install \\\n    --control-plane \\\n    --bundle",
		// A later cluster: registered with the control plane already logged in to,
		// so it names a bundle and no control plane.
		"kubenest platform install \\\n    --bundle",
		"--admin-email",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("install help does not show %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "--profile observability") {
		t.Errorf("install help advertises an unbuilt profile:\n%s", help)
	}
}

// The install help lists every flag the command accepts, so the flag surface
// can be read off the text an operator reads. Asserted both ways round: a
// name that came back — a removed switch, or a second spelling of one — shows
// up here as a name the surface does not have, and a name that disappeared
// shows up as a missing one.
func TestPlatformInstallHelpListsExactlyItsFlagSurface(t *testing.T) {
	root := NewRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"platform", "install", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("platform install help: %v", err)
	}

	want := map[string]bool{
		"admin-email": true, "agent": true, "backup-target": true, "bundle": true,
		"control-plane": true, "domain": true, "fleet-recipient": true, "ha": true,
		"help": true, "instance-id": true, "name": true,
		"org": true, "profile": true, "server": true, "ssh-key": true, "ssh-user": true,
		"storage-device": true,
	}
	got := map[string]bool{}
	inFlags := false
	for _, line := range strings.Split(output.String(), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "Flags:":
			inFlags = true
			continue
		case !inFlags, trimmed == "":
			continue
		case !strings.HasPrefix(trimmed, "-"):
			inFlags = false
			continue
		}
		for _, field := range strings.Fields(line) {
			field = strings.TrimSuffix(field, ",")
			if strings.HasPrefix(field, "--") {
				got[strings.TrimPrefix(field, "--")] = true
				break
			}
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("install help does not list --%s", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("install help lists --%s, which the install surface does not have", name)
		}
	}
}

// Every platform command is implemented now. What is asserted here is that
// each still refuses loudly rather than pretending: a command that cannot do
// its job must exit non-zero and say why.
func TestUpgradeRefusesWithoutItsRequiredFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"platform", "upgrade"}, "--cluster is required"},
		{[]string{"platform", "upgrade", "--cluster", "prod-1"}, "--to is required"},
		{[]string{"platform", "rollback"}, "--cluster is required"},
		{[]string{"platform", "diff", "--from", "1.0"}, "--from and --to"},
		{[]string{"cluster", "set-window"}, "--cluster is required"},
		// --now acts regardless of the window and --wait holds for it. Two
		// instructions that contradict each other are answered by asking, not
		// by picking one — the same rule --control-plane and --org follow.
		{[]string{"platform", "upgrade", "--cluster", "prod-1", "--to", "1.1", "--now", "--wait"}, "--now and --wait ask for opposite things"},
	} {
		root := NewRootCommand()
		root.SetArgs(c.args)
		err := root.Execute()
		if err == nil {
			t.Fatalf("%v must refuse", c.args)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: want %q, got: %v", c.args, c.want, err)
		}
	}
}

// An install that is going to register has to have somewhere to register, and
// it stops before it touches a host. The refusal has to name both ways on: a
// first cluster installs the control plane, and any other cluster is added
// against a login.
func TestInstallWithNowhereToRegisterNamesBothWaysOn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := NewRootCommand()
	root.SetArgs([]string{"platform", "install",
		"--bundle", "1.0", "--name", "x", "--server", "10.0.0.1", "--ha", "single-server"})
	err := root.Execute()
	if err == nil {
		t.Fatal("install with no control plane configured and no --control-plane must refuse")
	}
	if !strings.Contains(err.Error(), "kubenest login") {
		t.Errorf("the refusal must name the fix, got: %v", err)
	}
	if !strings.Contains(err.Error(), "kubenest platform install --control-plane") {
		t.Errorf("the refusal does not offer the other way on, got: %v", err)
	}
}

// The control plane being installed has exactly one organization, so --org
// has nothing to choose between. Two instructions that contradict each other
// are answered by asking, not by picking one.
func TestControlPlaneAndOrgAreMutuallyExclusive(t *testing.T) {
	f := InstallFlags{
		Bundle: "1.0", Name: "prod-1", Servers: []string{"10.0.1.10"},
		HATier: "single-server", ControlPlane: true, Org: "acme",
	}
	err := f.Validate()
	if err == nil {
		t.Fatal("--control-plane with --org must be refused: that control plane has one organization")
	}
	if !strings.Contains(err.Error(), "--org") || !strings.Contains(err.Error(), "--control-plane") {
		t.Errorf("the refusal does not name both flags: %v", err)
	}
}

// --domain and --admin-email describe the control plane's own serving address
// and its administrator. Neither means anything on a cluster being added to a
// fleet, and silently ignoring them would leave the operator believing they
// had configured something.
func TestControlPlaneOnlyFlagsNeedControlPlane(t *testing.T) {
	base := func() InstallFlags {
		return InstallFlags{
			Bundle: "1.0", Name: "prod-2", Servers: []string{"10.0.1.10"},
			HATier: "single-server",
		}
	}
	f := base()
	f.Domain = "kubenest.example.com"
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "--domain") {
		t.Errorf("--domain without --control-plane must be refused, got %v", err)
	}
	f = base()
	f.AdminEmail = "ops@example.com"
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "--admin-email") {
		t.Errorf("--admin-email without --control-plane must be refused, got %v", err)
	}
}

// The generated domain is what the console, the API and the hub are served
// under, and sslip.io answers <address>.sslip.io with that address: a first
// cluster therefore needs no DNS arranged before it can serve its console.
func TestControlPlaneDomainDefaultsToTheFirstServer(t *testing.T) {
	f := InstallFlags{Servers: []string{"10.0.1.10", "10.0.1.11"}}
	if got := f.controlPlaneDomain(); got != "10.0.1.10.sslip.io" {
		t.Errorf("default domain = %q, want the first server's address under sslip.io", got)
	}
	if got := f.controlPlaneAdminEmail(f.controlPlaneDomain()); got != "admin@10.0.1.10.sslip.io" {
		t.Errorf("default admin = %q", got)
	}

	f.Domain = "kubenest.example.com"
	if got := f.controlPlaneDomain(); got != "kubenest.example.com" {
		t.Errorf("--domain must win over the derived name, got %q", got)
	}
	f.AdminEmail = "ops@example.com"
	if got := f.controlPlaneAdminEmail(f.Domain); got != "ops@example.com" {
		t.Errorf("--admin-email must win over the derived account, got %q", got)
	}
}

// The load-bearing property of --control-plane at the command surface: the
// bundle manifest comes from this binary, not from a control plane. Asking
// for a version the binary does not carry must fail on THAT, having never
// looked for a control plane or a login.
func TestControlPlaneReadsTheBundleFromTheBinaryNotAControlPlane(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := NewRootCommand()
	root.SetArgs([]string{"platform", "install", "--control-plane",
		"--bundle", "99.9", "--name", "x", "--server", "10.0.0.1", "--ha", "single-server"})
	err := root.Execute()
	if err == nil {
		t.Fatal("a bundle this CLI does not carry must be refused")
	}
	if strings.Contains(err.Error(), "kubenest login") || strings.Contains(err.Error(), "no control plane configured") {
		t.Errorf("a --control-plane install went looking for a control plane to log in to: %v", err)
	}
	if !strings.Contains(err.Error(), "does not carry bundle") {
		t.Errorf("the refusal blames the wrong thing: %v", err)
	}
}

func TestUninstallDemandsConfirm(t *testing.T) {
	root := NewRootCommand()
	root.SetArgs([]string{"platform", "uninstall"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Errorf("uninstall without --confirm must refuse, got: %v", err)
	}
}

// windowServer answers the routes `cluster set-window` uses: the orgs and
// clusters the cluster NAME is resolved through, and the window it reads and
// writes. putStatus is what the write answers with, so a test can drive the
// control plane's own refusals as well as its successes.
func windowServer(t *testing.T, get string, putStatus int, put string) (*api.Client, *int, *any) {
	t.Helper()
	puts := 0
	var revision any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/orgs":
			io.WriteString(w, `[{"id":"o1","name":"acme","slug":"acme"}]`)
		case r.URL.Path == "/api/v1/orgs/o1/clusters":
			io.WriteString(w, `{"data":[{"id":"c1","name":"prod-1","status":"connected","org_id":"o1"}],"has_more":false}`)
		case r.URL.Path == "/api/v1/clusters/c1/maintenance-window" && r.Method == http.MethodGet:
			io.WriteString(w, get)
		case r.URL.Path == "/api/v1/clusters/c1/maintenance-window" && r.Method == http.MethodPut:
			puts++
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			json.Unmarshal(raw, &body)
			revision = body["revision"]
			if putStatus >= 400 {
				w.WriteHeader(putStatus)
			}
			io.WriteString(w, put)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := api.New(srv.URL, api.WithToken("knp_test"))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return c, &puts, &revision
}

const (
	// A cluster already at revision 4: the command reads this and must write
	// carrying 4.
	readWindowJSON = `{"window":{"days":["sat","sun"],"start":"02:00","end":"06:00","timezone":"Asia/Kolkata"},"revision":4,"state":"applying","applied_revision":3,"reject_reason":null,"warning":null}`
	// What the write answers with: the new window at revision 5.
	writtenWindowJSON = `{"window":{"days":["sat","sun"],"start":"02:00","end":"06:00","timezone":"Asia/Kolkata"},"revision":5,"state":"applying","applied_revision":3,"reject_reason":null,"warning":null}`
)

func setWindowSpec() window.Spec {
	return window.Spec{Days: []string{"sat", "sun"}, Start: "02:00", End: "06:00", Timezone: "Asia/Kolkata"}
}

// THE THREE STATES ARE PRINTED SEPARATELY, AND A WINDOW THAT IS NOT ACTIVE IS
// NOT REPORTED AS IN FORCE.
//
// This is the operator-facing half of kn-nqj: an operator who set a window and
// is told "the window is set" reads it as "upgrades are confined to it". Stored
// is not in force, and applying is not in force either — only the operator's
// acknowledgement of THIS revision makes it so.
func TestSetWindowPrintsThreeStates(t *testing.T) {
	t.Run("stored and applying", func(t *testing.T) {
		client, puts, revision := windowServer(t, readWindowJSON, http.StatusOK, writtenWindowJSON)

		var out bytes.Buffer
		if err := runWindow(context.Background(), &out, client, "prod-1", setWindowSpec()); err != nil {
			t.Fatal(err)
		}
		if *puts != 1 {
			t.Fatalf("the window was written %d time(s), want 1", *puts)
		}
		// The write carries the revision the command READ, or it is a silent
		// overwrite of whatever another operator stored in between.
		if got, ok := (*revision).(float64); !ok || int(got) != 4 {
			t.Errorf("the write carried revision %v, want the 4 that was read", *revision)
		}

		printed := out.String()
		if got := stateLineCount(printed); got != 3 {
			t.Errorf("the three states must be three separate lines, got %d:\n%s", got, printed)
		}
		for _, want := range []string{"stored:", "applying:", "active:", "revision 5", "NOT in force"} {
			if !strings.Contains(printed, want) {
				t.Errorf("the output is missing %q:\n%s", want, printed)
			}
		}
	})

	t.Run("active", func(t *testing.T) {
		active := `{"window":{"days":["sat","sun"],"start":"02:00","end":"06:00","timezone":"Asia/Kolkata"},"revision":5,"state":"active","applied_revision":5,"reject_reason":null,"warning":null}`
		client, _, _ := windowServer(t, readWindowJSON, http.StatusOK, active)

		var out bytes.Buffer
		if err := runWindow(context.Background(), &out, client, "prod-1", setWindowSpec()); err != nil {
			t.Fatal(err)
		}
		printed := out.String()
		if got := stateLineCount(printed); got != 3 {
			t.Errorf("the three states must be three separate lines, got %d:\n%s", got, printed)
		}
		if !strings.Contains(printed, "in force") || strings.Contains(printed, "NOT in force") {
			t.Errorf("an acknowledged revision is in force and must be reported as such:\n%s", printed)
		}
	})
}

// stateLineCount counts the lines that open with one of the three state names.
// A collapsed report — "stored: ... applying: ... active: ..." on one line —
// counts once, which is the point: three states a reader can point at.
func stateLineCount(printed string) int {
	count := 0
	for _, line := range strings.Split(printed, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, state := range []string{"stored:", "applying:", "active:"} {
			if strings.HasPrefix(trimmed, state) {
				count++
				break
			}
		}
	}
	return count
}

// The backup-overlap warning, and the two things it must not be confused with:
// a warning is NOT a refusal, and "unknown" is not "no overlap".
func TestSetWindowWarnsOnBackupOverlap(t *testing.T) {
	overlap := `{"window":{"days":["sat","sun"],"start":"02:30","end":"06:00","timezone":"UTC"},"revision":5,"state":"stored","applied_revision":null,"reject_reason":null,"warning":"this window starts at 02:30 UTC, inside the nightly backup that starts at 02:00 UTC and last took 60 minutes: a reboot during a backup marks that backup ineligible. A later window avoids it; an overlapping one is allowed."}`

	t.Run("an overlapping window warns and is still stored", func(t *testing.T) {
		client, puts, _ := windowServer(t, readWindowJSON, http.StatusOK, overlap)
		var out bytes.Buffer
		if err := runWindow(context.Background(), &out, client, "prod-1", setWindowSpec()); err != nil {
			t.Fatalf("an overlap is a warning, never a refusal: %v", err)
		}
		if *puts != 1 {
			t.Fatalf("the window was not stored")
		}
		printed := out.String()
		if !strings.Contains(printed, "Warning:") || !strings.Contains(printed, "inside the nightly backup") {
			t.Errorf("the overlap warning was not printed:\n%s", printed)
		}
	})

	t.Run("an unmeasured backup reads unknown", func(t *testing.T) {
		unknown := `{"window":{"days":["sat","sun"],"start":"04:00","end":"06:00","timezone":"UTC"},"revision":5,"state":"stored","applied_revision":null,"reject_reason":null,"warning":"unknown"}`
		client, _, _ := windowServer(t, readWindowJSON, http.StatusOK, unknown)
		var out bytes.Buffer
		if err := runWindow(context.Background(), &out, client, "prod-1", setWindowSpec()); err != nil {
			t.Fatal(err)
		}
		printed := out.String()
		if !strings.Contains(printed, "unknown") || !strings.Contains(printed, "not a passing check") {
			t.Errorf("an unmeasured backup is not a passing check and must say so:\n%s", printed)
		}
	})

	t.Run("no overlap says so", func(t *testing.T) {
		clear := `{"window":{"days":["sat","sun"],"start":"04:00","end":"06:00","timezone":"UTC"},"revision":5,"state":"active","applied_revision":5,"reject_reason":null,"warning":null}`
		client, _, _ := windowServer(t, readWindowJSON, http.StatusOK, clear)
		var out bytes.Buffer
		if err := runWindow(context.Background(), &out, client, "prod-1", setWindowSpec()); err != nil {
			t.Fatal(err)
		}
		printed := out.String()
		if strings.Contains(printed, "Warning") {
			t.Errorf("a null warning is the control plane saying it read the evidence and found no overlap:\n%s", printed)
		}
		if !strings.Contains(printed, "does not overlap the nightly backup") {
			t.Errorf("the absence of a warning must still be stated, not left blank:\n%s", printed)
		}
	})
}

// A WINDOW THIS CLI CANNOT REPRESENT IS REFUSED BEFORE ANYTHING IS SENT.
//
// The control plane enforces the same rules, and that is the point: the CLI
// must not accept what it would itself refuse to read back, and must not make
// the operator learn about it from a stored window that is not what they asked
// for.
func TestSetWindowRefusesAWindowTheCLICannotRepresent(t *testing.T) {
	client, puts, _ := windowServer(t, readWindowJSON, http.StatusOK, writtenWindowJSON)

	for _, c := range []struct {
		name string
		spec window.Spec
	}{
		{"an offset instead of an IANA name", window.Spec{Days: []string{"sat"}, Start: "02:00", End: "06:00", Timezone: "+05:30"}},
		{"a zero-length window", window.Spec{Days: []string{"sat"}, Start: "02:00", End: "02:00", Timezone: "UTC"}},
		{"a day that is not a day", window.Spec{Days: []string{"notaday"}, Start: "02:00", End: "06:00", Timezone: "UTC"}},
		{"no day at all", window.Spec{Start: "02:00", End: "06:00", Timezone: "UTC"}},
		{"a time that is not a time", window.Spec{Days: []string{"sat"}, Start: "25:00", End: "06:00", Timezone: "UTC"}},
		// A crossing window naming fewer than all seven days is read
		// differently by kured than by this CLI (pkg/window/kured_oracle_test.go),
		// so set-window must refuse it rather than store a window that means
		// something else on the cluster than it does here.
		{"a crossing window that does not name all seven days", window.Spec{Days: []string{"sat"}, Start: "22:00", End: "04:00", Timezone: "UTC"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := runWindow(context.Background(), &out, client, "prod-1", c.spec); err == nil {
				t.Fatal("the window must be refused, not stored")
			}
			if out.Len() != 0 {
				t.Errorf("a refused window must not print a stored one:\n%s", out.String())
			}
		})
	}
	if *puts != 0 {
		t.Errorf("%d window(s) reached the control plane, want none: the CLI refuses what it cannot represent", *puts)
	}
}

// If the CLI's rules and the control plane's ever drift, the drift must reach
// the operator as a refusal with the reason — never as a success over a window
// that was not stored.
func TestSetWindowSurfacesTheControlPlanesRefusal(t *testing.T) {
	client, puts, _ := windowServer(t, readWindowJSON, http.StatusUnprocessableEntity,
		`{"detail": "timezone 'Mars/Phobos' is not an IANA name (Asia/Kolkata, Europe/Berlin, UTC)"}`)

	var out bytes.Buffer
	err := runWindow(context.Background(), &out, client, "prod-1", setWindowSpec())
	if err == nil {
		t.Fatal("a 422 from the control plane must refuse")
	}
	if !strings.Contains(err.Error(), "Mars/Phobos") {
		t.Errorf("the refusal must carry the control plane's reason, got: %v", err)
	}
	if *puts != 1 {
		t.Errorf("the write was attempted %d time(s), want 1", *puts)
	}
	if out.Len() != 0 {
		t.Errorf("nothing was stored, so no stored window may be reported:\n%s", out.String())
	}
}
