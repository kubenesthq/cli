//go:build e2e

// T3.2's gate: the host update policy, on REAL hosts with a real install.
//
// What it asserts, which is the gate:
//
//  1. an install leaves /etc/apt/apt.conf.d/52kubenest-unattended-upgrades and
//     /etc/needrestart/conf.d/99-kubenest.conf on the server AND the agent —
//     two different stages write them, so both are checked;
//  2. `unattended-upgrade --dry-run --debug` names security origins only (no
//     -updates), in one run: this is the origins apt would really install
//     from, not the file the installer wrote;
//  3. `apt-config dump` reports Unattended-Upgrade::Automatic-Reboot "false" —
//     the EFFECTIVE value, which is what survives a file that sorts after ours;
//  4. probe P6: after a real unattended upgrade, `needrestart -b -r l` does NOT
//     list k3s.service / k3s-agent.service as needing a restart.
//
// Step 4 records P6's answer rather than assuming it. The ignore list ships
// either way — that is P6's stated fallback (plan 9.1) — so the step asserts
// the with-list behaviour, logs what needrestart does WITHOUT the list (the
// answer to P6 itself), and puts the file back. On a host where P6 answers
// "needrestart cannot restart k3s at all", step 4 passes because k3s is never
// listed; on a host where it answers "yes", the log line says the ignore entry
// is what stopped it, and removing that entry fails this step.
//
// Run from the umbrella workspace against a FRESH lab node pair — the test
// performs the install itself, so a host that already carries a cluster is not
// a valid target:
//
//	source lab/hetzner/.lab-env.sh
//	export KUBENEST_CONTROL_PLANE=http://localhost:8000
//	export KUBENEST_CLI_TOKEN=knp_...
//	export KUBENEST_LAB_NODE2_IP=...        # the agent host
//	cd kubenest-cli && go test -tags e2e -run TestHostPolicyGate -v -timeout 90m ./e2e/
package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/hostpolicy"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/uninstall"
)

func TestHostPolicyGate(t *testing.T) {
	env := gateEnvironment(t)
	agent := os.Getenv("KUBENEST_LAB_NODE2_IP")
	if agent == "" {
		t.Skip("KUBENEST_LAB_NODE2_IP not set: the policy must land on the agent host too — ./scripts/ephemeral-env.sh up --profile host with two nodes")
	}
	ctx := context.Background()

	client, err := api.New(env.controlPlane, api.WithToken(env.token))
	if err != nil {
		t.Fatal(err)
	}

	// Step 2 reads a line unattended-upgrades logs, so the reading is checked
	// against that format BEFORE a fifteen-minute install depends on it: a
	// gate whose own parser is wrong would fail on the host for the wrong
	// reason. The fixture is the format the installed script writes
	// (logging.info with a ", "-joined list of o=<origin>,a=<archive>).
	t.Run("0. the origins assertion matches what unattended-upgrade logs", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			out    string
			bad    []string
			listed bool
		}{
			{
				name:   "the installer's policy",
				out:    "2026-09-25 06:31:02,114 INFO Allowed origins are: o=Ubuntu,a=noble-security\n",
				listed: true,
			},
			{
				name:   "the distro's own list survived",
				out:    "2026-09-25 06:31:02,114 INFO Allowed origins are: o=Ubuntu,a=noble, o=Ubuntu,a=noble-security, o=UbuntuESM,a=noble-infra-security\n",
				bad:    []string{"o=Ubuntu,a=noble"},
				listed: true,
			},
			{
				name:   "-updates was allowed",
				out:    "Allowed origins are: o=Ubuntu,a=noble-security, o=Ubuntu,a=noble-updates\n",
				bad:    []string{"o=Ubuntu,a=noble-updates"},
				listed: true,
			},
			{name: "no line at all", out: "nothing to say about origins\n"},
		} {
			bad, listed := nonSecurityOrigins(tc.out)
			if listed != tc.listed {
				t.Errorf("%s: listed = %v, want %v", tc.name, listed, tc.listed)
			}
			if strings.Join(bad, "|") != strings.Join(tc.bad, "|") {
				t.Errorf("%s: non-security origins = %q, want %q", tc.name, bad, tc.bad)
			}
		}
	})

	t.Run("install the cluster", func(t *testing.T) {
		bundle := fetchBundle(t, client, env.bundle)
		s, _ := session(t, env, t.TempDir()+"/install.json", bundle, install.Options{
			Bundle: env.bundle, Name: env.cluster, HATier: "single-server",
			Servers: []string{env.server}, Agents: []string{agent},
			SSHUser: env.sshUser, SSHKey: env.sshKey,
			StorageDevice: env.storageDevice,
		})
		defer s.Close()
		if _, err := install.Execute(ctx, s, install.Plan(s)); err != nil {
			t.Fatalf("installing bundle %s on %s and %s: %v", env.bundle, env.server, agent, err)
		}
		t.Logf("installed bundle %s with the agent host %s", env.bundle, agent)
	})

	hosts := hostPolicyHosts(t, env, agent)

	t.Run("1. the policy is on the server and the agent", func(t *testing.T) {
		for _, host := range hosts {
			assertHostFile(t, ctx, host, hostpolicy.APTConfPath, string(hostpolicy.DropIn()))
			assertHostFile(t, ctx, host, hostpolicy.NeedrestartConfPath, string(hostpolicy.NeedrestartConf()))
		}
	})

	t.Run("2. unattended-upgrade selects security updates only", func(t *testing.T) {
		for _, host := range hosts {
			refreshAptLists(t, ctx, host)
			// One run, no retry: which origins apt would install from is what
			// the policy decides, and a second run cannot decide differently.
			res := hostCommand(t, ctx, host, "sudo unattended-upgrade --dry-run --debug")
			out := res.Stdout + res.Stderr
			if res.ExitCode != 0 {
				t.Fatalf("on %s: unattended-upgrade --dry-run --debug exited %d:\n%s",
					host.Address, res.ExitCode, firstLines(out, 20))
			}
			bad, listed := nonSecurityOrigins(out)
			switch {
			case !listed:
				t.Errorf("on %s: the dry run names no allowed origins at all, so it proves nothing about the policy:\n%s",
					host.Address, firstLines(out, 20))
			case len(bad) > 0:
				t.Errorf("on %s: the dry run would install from %s, which is not a security pocket — the policy is not in effect:\n%s",
					host.Address, strings.Join(bad, ", "), firstLines(out, 20))
			}
		}
	})

	t.Run("3. the effective APT policy never reboots by itself", func(t *testing.T) {
		for _, host := range hosts {
			res := hostCommand(t, ctx, host, "apt-config dump")
			if res.ExitCode != 0 {
				t.Fatalf("on %s: apt-config dump exited %d: %s",
					host.Address, res.ExitCode, strings.TrimSpace(res.Stderr))
			}
			// The dump is what apt (and unattended-upgrades) really sees after
			// reading every file in /etc/apt/apt.conf.d, so it is the only
			// reading that cannot be fooled by a file sorting after ours.
			const want = `Unattended-Upgrade::Automatic-Reboot "false";`
			if !strings.Contains(res.Stdout, want) {
				t.Errorf("on %s: the EFFECTIVE Automatic-Reboot is not %q, so APT may reboot this node outside the reboot window. Observed:\n%s",
					host.Address, want, linesMatching(res.Stdout, "Automatic-Reboot"))
			}
		}
	})

	t.Run("4. P6: needrestart does not restart k3s", func(t *testing.T) {
		for _, host := range hosts {
			// A real unattended upgrade first, so needrestart has something to
			// react to. What it did is part of the observation: on a host with
			// nothing pending, needrestart has nothing to restart either way.
			res := hostCommand(t, ctx, host, "sudo unattended-upgrade -d")
			t.Logf("on %s: unattended-upgrade -d exited %d:\n%s",
				host.Address, res.ExitCode, firstLines(res.Stdout+res.Stderr, 10))

			// The assertion, with the ignore list in place.
			listed, raw := needrestartWouldRestart(t, ctx, host)
			for _, unit := range []string{hostpolicy.K3sUnit, hostpolicy.K3sAgentUnit} {
				if listed[unit] {
					t.Errorf("on %s: needrestart would restart %s after an unattended upgrade — P6 answered \"yes\" and the ignore list did not stop it. Observed:\n%s",
						host.Address, unit, raw)
				}
			}

			// P6 itself, recorded rather than asserted: needrestart reads
			// *.conf only, so moving our file aside takes the entries out for
			// the length of one observation. The file is put back before this
			// subtest ends, assertion failure or not.
			t.Logf("on %s: P6: %s", host.Address, probeNeedrestartWithoutTheIgnoreList(t, ctx, host))
		}
	})
}

// hostPolicyHosts dials the lab's server and its agent. Two hosts, because the
// policy is written by two different stages and a policy that only reached the
// server would leave the agent free to reboot itself.
func hostPolicyHosts(t *testing.T, env gateEnv, agent string) []uninstall.Node {
	t.Helper()
	opts := sshx.Options{
		User:           env.sshUser,
		KeyPath:        env.sshKey,
		KnownHostsPath: filepath.Join(t.TempDir(), "known_hosts"),
		DialTimeout:    15 * time.Second,
	}
	var hosts []uninstall.Node
	for i, address := range []string{env.server, agent} {
		role := uninstall.RoleServer
		if i == 1 {
			role = uninstall.RoleAgent
		}
		endpoint, err := sshx.Resolve(address, opts)
		if err != nil {
			t.Fatal(err)
		}
		client, err := sshx.Dial(context.Background(), endpoint, opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { client.Close() })
		hosts = append(hosts, uninstall.Node{Address: address, Role: role, Runner: client})
	}
	return hosts
}

// hostCommand runs one command on a host. A transport error is fatal (the
// host is unreachable, and every later assertion would be noise); a non-zero
// exit comes back to the caller, which is where the decision about what it
// means belongs.
func hostCommand(t *testing.T, ctx context.Context, host uninstall.Node, command string) sshx.Result {
	t.Helper()
	res, err := host.Runner.Run(ctx, command)
	if err != nil {
		t.Fatalf("on %s: %q: %v", host.Address, command, err)
	}
	return res
}

// assertHostFile compares a file on a real host with what this build renders.
func assertHostFile(t *testing.T, ctx context.Context, host uninstall.Node, path, want string) {
	t.Helper()
	res := hostCommand(t, ctx, host, "sudo cat "+path)
	if res.ExitCode != 0 {
		t.Errorf("on %s: %s is not there (%s). The k3s stages write it, so either this host was installed before T3.2 or with a different binary",
			host.Address, path, strings.TrimSpace(res.Stderr))
		return
	}
	if res.Stdout != want {
		t.Errorf("on %s: %s does not hold what this build writes:\n%s", host.Address, path, res.Stdout)
	}
}

// refreshAptLists makes the package lists current, which steps 2 and 4 need.
// A mirror hiccup is not a policy finding, so it is logged rather than fatal:
// the assertions are about the origins the host would install from, and those
// are readable from whatever lists the host has.
func refreshAptLists(t *testing.T, ctx context.Context, host uninstall.Node) {
	t.Helper()
	if res := hostCommand(t, ctx, host, "sudo apt-get update -q"); res.ExitCode != 0 {
		t.Logf("on %s: apt-get update exited %d, continuing with the lists the host has: %s",
			host.Address, res.ExitCode, firstLines(res.Stderr, 3))
	}
}

// needrestartWouldRestart reads the services needrestart would restart out of
// its batch output (one `NEEDRESTART-SVC:` line per service). The raw output
// comes back with it: an assertion about what a host said has to be able to
// show what the host said.
func needrestartWouldRestart(t *testing.T, ctx context.Context, host uninstall.Node) (map[string]bool, string) {
	t.Helper()
	res := hostCommand(t, ctx, host, "sudo needrestart -b -r l 2>&1")
	raw := strings.TrimSpace(res.Stdout)
	if raw == "" {
		t.Fatalf("on %s: needrestart -b -r l exited %d and said nothing, so nothing can be asserted about k3s",
			host.Address, res.ExitCode)
	}
	listed := map[string]bool{}
	for _, line := range strings.Split(raw, "\n") {
		if unit, ok := strings.CutPrefix(strings.TrimSpace(line), "NEEDRESTART-SVC:"); ok {
			listed[strings.TrimSpace(unit)] = true
		}
	}
	return listed, raw
}

// probeNeedrestartWithoutTheIgnoreList answers P6 on this host and returns the
// answer as a sentence for the log. needrestart reads /etc/needrestart/conf.d
// /*.conf, so renaming the file aside removes the entries without touching what
// is in them; the rename is undone before returning, on every path.
func probeNeedrestartWithoutTheIgnoreList(t *testing.T, ctx context.Context, host uninstall.Node) string {
	t.Helper()
	aside := hostpolicy.NeedrestartConfPath + ".p6-off"
	if res := hostCommand(t, ctx, host, "sudo mv "+hostpolicy.NeedrestartConfPath+" "+aside); res.ExitCode != 0 {
		return fmt.Sprintf("could not move %s aside to observe P6: %s",
			hostpolicy.NeedrestartConfPath, strings.TrimSpace(res.Stderr))
	}
	defer func() {
		if res := hostCommand(t, ctx, host, "sudo mv "+aside+" "+hostpolicy.NeedrestartConfPath); res.ExitCode != 0 {
			t.Errorf("on %s: putting %s back failed (%s): restore it by hand before running anything else on this host",
				host.Address, hostpolicy.NeedrestartConfPath, strings.TrimSpace(res.Stderr))
		}
	}()

	listed, raw := needrestartWouldRestart(t, ctx, host)
	var found []string
	for _, unit := range []string{hostpolicy.K3sUnit, hostpolicy.K3sAgentUnit} {
		if listed[unit] {
			found = append(found, unit)
		}
	}
	if len(found) > 0 {
		return fmt.Sprintf("needrestart WOULD restart %s without the ignore entry — P6 answered \"yes\", and 99-kubenest.conf is what stops it. Observed:\n%s",
			strings.Join(found, ", "), raw)
	}
	return fmt.Sprintf("needrestart would not restart k3s or k3s-agent even with the ignore list moved aside — P6 answered \"no\" on this host. Observed:\n%s",
		raw)
}

// nonSecurityOrigins reads the origins unattended-upgrades resolved from the
// whole configuration — the `Allowed origins are:` line of a dry run — and
// returns the ones that are not security pockets, plus whether the line was
// there at all:
//
//	Allowed origins are: o=Ubuntu,a=noble-security
//
// That line is what the tool itself computed, so it cannot be satisfied by a
// file the installer wrote while another file undoes it: an -updates archive
// does not end in -security, and neither does the release pocket, so a list
// still carrying them comes back here. This is the planted negative step 2
// exists to catch.
func nonSecurityOrigins(out string) (bad []string, listed bool) {
	for _, line := range strings.Split(out, "\n") {
		_, rest, ok := strings.Cut(line, "Allowed origins are:")
		if !ok {
			continue
		}
		listed = true
		for _, origin := range strings.Split(strings.TrimSpace(rest), ", ") {
			origin = strings.TrimSpace(origin)
			archive := origin
			if _, after, ok := strings.Cut(origin, "a="); ok {
				archive = strings.TrimSpace(after)
			}
			if !strings.HasSuffix(archive, "-security") {
				bad = append(bad, origin)
			}
		}
	}
	return bad, listed
}

// firstLines is the head of a command's output, for a failure message that
// stays readable.
func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "…")
	}
	return strings.Join(lines, "\n")
}

// linesMatching returns the lines of a dump that mention a key, or says so when
// there are none — "the setting is absent" is part of the observation.
func linesMatching(dump, key string) string {
	var out []string
	for _, line := range strings.Split(dump, "\n") {
		if strings.Contains(line, key) {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return "(no line mentioning " + key + " at all)"
	}
	return strings.Join(out, "\n")
}
