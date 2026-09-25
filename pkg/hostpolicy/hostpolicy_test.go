package hostpolicy

import (
	"context"
	"io"
	"path"
	"strings"
	"sync"
	"testing"

	"kubenest.io/cli/pkg/sshx"
)

// fakeHost is a target host with just enough filesystem for the apply path to
// be exercised end to end: it answers the reads, applies the writes, and
// records both. A fake that only recorded commands could not tell an
// idempotent apply from one that rewrites the same bytes every time — the
// second apply's decision is made from what the first one wrote.
type fakeHost struct {
	mu    sync.Mutex
	files map[string]string
	runs  []execution
	// dump is what `apt-config dump` answers with.
	dump string
}

// execution is one call the code under test made. The streamed payload is
// kept apart from the command: an assertion about where the content went
// needs to tell stdin from the argument list.
type execution struct {
	command string
	stdin   string
}

func newFakeHost() *fakeHost { return &fakeHost{files: map[string]string{}} }

func (f *fakeHost) Run(_ context.Context, command string) (sshx.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, execution{command: command})
	switch {
	case strings.HasPrefix(command, "sudo -n cat "):
		file, _, _ := strings.Cut(strings.TrimPrefix(command, "sudo -n cat "), " ")
		// The command ends in `|| true`, so a file that is not there reads as
		// empty — which is exactly what the caller treats as "not our
		// content, write it".
		return sshx.Result{Stdout: f.files[file]}, nil
	case strings.HasPrefix(command, "apt-config dump"):
		return sshx.Result{Stdout: f.dump}, nil
	}
	return sshx.Result{}, nil
}

func (f *fakeHost) RunInput(_ context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return sshx.Result{}, err
	}
	_, dest := writeTarget(command)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, execution{command: command, stdin: string(payload)})
	f.files[dest] = string(payload)
	return sshx.Result{}, nil
}

// writeTarget pulls the temp file and the final path out of the write
// command. Parsing the command is deliberate: the shape IS the discipline
// under test — a temp file created whole and renamed into place — so the fake
// applies whatever the code sent rather than what a helper assumed it sent.
func writeTarget(command string) (tmp, dest string) {
	_, rest, ok := strings.Cut(command, "mv -f ")
	if !ok {
		return "", ""
	}
	tmp, rest, _ = strings.Cut(rest, " ")
	dest, _, _ = strings.Cut(rest, " ")
	return tmp, dest
}

func (f *fakeHost) file(path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.files[path]
	return content, ok
}

func (f *fakeHost) executions() []execution {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]execution(nil), f.runs...)
}

// aptDirectives returns the lines of an apt.conf file that apt would act on,
// with comments and indentation removed, so the assertions below are about
// what apt reads rather than about how the file is laid out.
func aptDirectives(content string) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// listBlock returns the quoted entries of `Key { ... };` in an apt.conf
// document.
func listBlock(t *testing.T, directives []string, key string) []string {
	t.Helper()
	var entries []string
	in := false
	for _, line := range directives {
		switch {
		case in && line == "};":
			return entries
		case in:
			entries = append(entries, strings.Trim(strings.TrimSuffix(line, ";"), `"`))
		case line == key+" {":
			in = true
		}
	}
	t.Fatalf("the %s list is missing or never closed: %q", key, directives)
	return nil
}

// assignment returns the value of `Key "value";` in an apt.conf document. A
// key that only shares a prefix (Unattended-Upgrade::Automatic-Reboot-Time
// for Unattended-Upgrade::Automatic-Reboot) is a different key and does not
// match.
func assignment(directives []string, key string) (string, bool) {
	for _, line := range directives {
		if rest, ok := strings.CutPrefix(line, key+" "); ok {
			return strings.Trim(strings.TrimSuffix(rest, ";"), `"`), true
		}
	}
	return "", false
}

// The whole "installs nothing but security updates" claim, as bytes. The
// origin list is CLEARED first because apt APPENDS to a list a later file
// re-opens: Ubuntu's own 50unattended-upgrades names the release pocket and
// the ESM pockets, and the name of our file alone would not take them away.
func TestDropInContentIsSecurityOnlyAndNeverReboots(t *testing.T) {
	directives := aptDirectives(string(DropIn()))

	// Exactly one origin, and it is the security pocket. -updates (and the
	// release pocket, which Ubuntu's own rule allows by name) is what must
	// not be here; the list is compared as a whole so an entry added
	// alongside ours fails rather than passing a "contains -security" check.
	origins := listBlock(t, directives, "Unattended-Upgrade::Allowed-Origins")
	const securityOrigin = "${distro_id}:${distro_codename}-security"
	if len(origins) != 1 || origins[0] != securityOrigin {
		t.Fatalf("the policy must allow exactly %q, got %q", securityOrigin, origins)
	}
	if !containsDirective(directives, "#clear Unattended-Upgrade::Allowed-Origins;") {
		t.Errorf("the distro's own origin list must be cleared before ours is set, or its entries survive underneath ours:\n%s",
			strings.Join(directives, "\n"))
	}

	reboot, ok := assignment(directives, "Unattended-Upgrade::Automatic-Reboot")
	if !ok {
		t.Fatalf("the policy must state Automatic-Reboot explicitly: %q", directives)
	}
	if reboot != "false" {
		t.Errorf(`Automatic-Reboot must be "false" — the reboot decision is the window's and kured's — got %q`, reboot)
	}
}

func containsDirective(directives []string, want string) bool {
	for _, line := range directives {
		if line == want {
			return true
		}
	}
	return false
}

// apt reads /etc/apt/apt.conf.d in lexicographic order, so the name is half
// the mechanism: only a file that sorts after Ubuntu's own rule can clear the
// list it set. (The other half is the #clear in the content, because a later
// file APPENDS to a list instead of replacing it.)
func TestDropInNameSortsAfterTheDistroRule(t *testing.T) {
	const distroRule = "50unattended-upgrades"
	ours := path.Base(APTConfPath)
	if strings.Compare(ours, distroRule) <= 0 {
		t.Errorf("the drop-in reads as %q, which does not sort after %q — apt would read the distro's rule last",
			ours, distroRule)
	}
}

// Both k3s units are on needrestart's ignore list, and this test does not
// depend on what needrestart can do on a real host: the list ships either way,
// because that is the plan's stated fallback (9.1) for probe P6. The
// controller records the answer P6 observed on real hosts.
func TestNeedrestartIgnoresBothK3sUnits(t *testing.T) {
	conf := string(NeedrestartConf())
	for _, unit := range []string{K3sUnit, K3sAgentUnit} {
		entry := `qr(^` + strings.ReplaceAll(unit, ".", `\.`) + `$)`
		if !strings.Contains(conf, "override_rc") || !strings.Contains(conf, entry) {
			t.Errorf("needrestart must never restart %s; the entry %s is missing:\n%s", unit, entry, conf)
		}
	}
	// Adding to the override map, never replacing it: `$nrconf{override_rc} =
	// { ... }` would drop needrestart's own defaults (dbus, display managers,
	// systemd-logind) and start restarting services Ubuntu deliberately does
	// not restart.
	if strings.Contains(conf, "$nrconf{override_rc} =") {
		t.Errorf("the drop-in must add to needrestart's override map, not replace it:\n%s", conf)
	}
}

// The second apply is the one that matters: it reads back what the first one
// wrote and must change nothing at all — not the file, not even a rewrite of
// the same bytes.
func TestApplyIsIdempotent(t *testing.T) {
	host := newFakeHost()
	ctx := context.Background()

	if err := Apply(ctx, host); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ path, content string }{
		{APTConfPath, string(DropIn())},
		{NeedrestartConfPath, string(NeedrestartConf())},
	} {
		got, ok := host.file(want.path)
		if !ok {
			t.Fatalf("the first apply did not write %s", want.path)
		}
		if got != want.content {
			t.Errorf("%s holds\n%s\nwant\n%s", want.path, got, want.content)
		}
	}
	// The discipline the apply shares with k3s.WriteManifest: the content
	// travels over stdin and never in the command string, and the file only
	// ever appears whole, by rename — apt reads the whole directory, so a
	// half-written policy is not acceptable even for a moment.
	writes := 0
	for _, ex := range host.executions() {
		if !strings.Contains(ex.command, "mv -f ") {
			continue
		}
		writes++
		if !strings.Contains(ex.command, ".tmp") {
			t.Errorf("a write renames nothing into place: %q", ex.command)
		}
		if ex.stdin == "" {
			t.Errorf("a write streamed no content: %q", ex.command)
		}
	}
	if writes != 2 {
		t.Fatalf("the first apply must write both files whole, wrote %d", writes)
	}

	before := len(host.executions())
	if err := Apply(ctx, host); err != nil {
		t.Fatal(err)
	}
	second := host.executions()[before:]
	if len(second) == 0 {
		t.Fatal("the second apply ran nothing at all: it must read the files back to decide")
	}
	for _, ex := range second {
		if strings.Contains(ex.command, "install ") || strings.Contains(ex.command, "mv -f ") {
			t.Errorf("an unchanged file was rewritten: %q", ex.command)
		}
	}
}

// The policy check reads the EFFECTIVE configuration with `apt-config dump`,
// so a later file re-enabling -updates or Automatic-Reboot is what is seen —
// not the file the installer wrote. This is the assertion verify.go makes on
// every host after an install, and the reboot half is what preflight refuses a
// host for.
func TestTheEffectivePolicyIsWhatTheHostsAptConfigurationSays(t *testing.T) {
	const securom = `Unattended-Upgrade::Allowed-Origins:: "${distro_id}:${distro_codename}-security";` + "\n"
	for _, tc := range []struct {
		name    string
		dump    string
		problem string // what NonCompliance must name; empty means it must comply
		reboots bool
	}{
		{
			name: "the installer's own policy",
			dump: `Unattended-Upgrade::Allowed-Origins "";` + "\n" + securom +
				`Unattended-Upgrade::Automatic-Reboot "false";` + "\n" +
				// A different key that shares the prefix: it must not be read
				// as the reboot switch.
				`Unattended-Upgrade::Automatic-Reboot-Time "02:00";` + "\n",
		},
		{
			name: "a later file put the release pocket back",
			dump: `Unattended-Upgrade::Allowed-Origins "";` + "\n" + securom +
				`Unattended-Upgrade::Allowed-Origins:: "${distro_id}:${distro_codename}";` + "\n" +
				`Unattended-Upgrade::Automatic-Reboot "false";` + "\n",
			problem: "${distro_id}:${distro_codename}",
		},
		{
			name: "a customer image lets APT reboot by itself",
			dump: securom +
				`Unattended-Upgrade::Automatic-Reboot "true";` + "\n",
			problem: "Automatic-Reboot",
			reboots: true,
		},
		{
			name:    "the origins were cleared and nothing put back",
			dump:    `Unattended-Upgrade::Automatic-Reboot "false";` + "\n",
			problem: "no unattended-upgrade origins",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newFakeHost()
			host.dump = tc.dump

			effective, err := ReadEffective(context.Background(), host)
			if err != nil {
				t.Fatal(err)
			}
			if effective.AutomaticReboot != tc.reboots {
				t.Errorf("AutomaticReboot = %v, want %v (read from %q)", effective.AutomaticReboot, tc.reboots, tc.dump)
			}
			problem := effective.NonCompliance()
			if tc.problem == "" {
				if problem != "" {
					t.Errorf("the installer's own policy must comply, got %q", problem)
				}
				return
			}
			if !strings.Contains(problem, tc.problem) {
				t.Errorf("the finding must name %q, got %q", tc.problem, problem)
			}
		})
	}
}
