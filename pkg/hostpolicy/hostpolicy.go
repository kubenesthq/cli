// Package hostpolicy writes the host-level update policy the platform owns on
// every node the installer touches: the APT unattended-upgrades drop-in and
// needrestart's ignore list.
//
// The policy is TWO files, not a configuration option, because both are read
// by software outside Kubernetes:
//
//   - /etc/apt/apt.conf.d/52kubenest-unattended-upgrades pins security updates
//     only and Automatic-Reboot "false". Ubuntu 24.04's default already does
//     the second and roughly the first, but a default we did not write is not
//     a policy we chose, versioned or can test — two customers on two images
//     would otherwise get two behaviours and neither would be ours (plan 7.4).
//     With this file the reboot decision belongs entirely to the reboot window
//     (T3.1) and to kured (T3.3). k3s and its container runtime are NOT APT
//     packages, so nothing here holds them: the policy installs nothing but
//     security updates, and that is all it claims.
//
//   - /etc/needrestart/conf.d/99-kubenest.conf puts k3s.service and
//     k3s-agent.service on needrestart's ignore list, so a package update
//     cannot restart k3s underneath a cluster whose window is closed — on a
//     single-server cluster that restart is an API outage outside the window.
//
// Probe P6 (plan 9.1) asks whether needrestart CAN restart k3s after an
// unattended upgrade. The ignore list ships for both units REGARDLESS of the
// answer, because that is P6's stated fallback (9.1) and it makes this code
// independent of the probe's outcome; the controller records the answer P6
// observed on real hosts alongside this package when it verifies. The e2e gate
// (e2e/host_policy_test.go, TestHostPolicyGate step 4) is where the observation
// is made on a host, and it states in its failure message what it saw.
//
// The write discipline is k3s.WriteManifest's, for the same reason: apt reads
// the whole apt.conf.d directory and needrestart reads the whole file, so a
// half-written policy must never be observable — the content goes to a temp
// name at the final mode, is renamed into place, and an unchanged file is
// never rewritten (a rewrite is a window in which the file is short).
package hostpolicy

import (
	"context"
	"fmt"
	"path"
	"strings"

	"kubenest.io/cli/pkg/k3s"
)

const (
	// APTConfPath is the unattended-upgrades drop-in. The name sorts after
	// Ubuntu's 50unattended-upgrades because apt reads the directory in
	// lexicographic order, and a file that sorts last is the one whose
	// directives are read last.
	APTConfPath = "/etc/apt/apt.conf.d/52kubenest-unattended-upgrades"
	// NeedrestartConfPath is the needrestart drop-in. needrestart reads
	// /etc/needrestart/conf.d/*.conf in lexical order and evaluates each file
	// as a Perl snippet.
	NeedrestartConfPath = "/etc/needrestart/conf.d/99-kubenest.conf"

	// K3sUnit and K3sAgentUnit are the units needrestart must never restart:
	// the server and the agent are the same binary under two unit names, and
	// which one a host runs is decided by its role.
	K3sUnit      = "k3s.service"
	K3sAgentUnit = "k3s-agent.service"

	// SecurityOrigin is the single origin the policy installs from. It is
	// written with the placeholders apt stores verbatim: unattended-upgrades
	// substitutes ${distro_id} and ${distro_codename} at run time, so these
	// are the bytes both the file and `apt-config dump` carry.
	SecurityOrigin = "${distro_id}:${distro_codename}-security"

	// allowedOriginsKey and automaticRebootKey are the two APT configuration
	// keys the policy owns; the reader below and the tests both name them.
	allowedOriginsKey  = "Unattended-Upgrade::Allowed-Origins"
	automaticRebootKey = "Unattended-Upgrade::Automatic-Reboot"

	// aptConfigDump is how the EFFECTIVE configuration is read back. It is
	// the value after every file in /etc/apt/apt.conf.d has been read in
	// order, which is what unattended-upgrades itself sees — the file the
	// installer wrote is not evidence that the policy survived.
	aptConfigDump = "apt-config dump"

	// confMode is the mode both files are installed with. They are policy,
	// not secrets, and both directories are world-readable; root owns them.
	confMode = "0644"
)

// aptDropIn is the rendered 52kubenest-unattended-upgrades.
//
// The #clear is load-bearing and is not an optimisation. apt APPENDS to a
// list when a later file re-opens it — the name order decides who reads last,
// not who replaces whom — so without the clear Ubuntu's own
// 50unattended-upgrades entries (the release pocket, the ESM pockets) would
// survive underneath ours and the host would keep installing more than
// security updates while this file looked correct.
const aptDropIn = `// The KubeNest host update policy. Written by the installer (and, for
// existing 1.1 clusters, by the 1.2 upgrade host step); do not edit it, it is
// overwritten on every run.
//
// 1. Security updates only. APT's list is cleared first because a later file
//    APPENDS to a list rather than replacing it, so the clear is what removes
//    the release pocket and the ESM pockets the distro's own rule allows.
// 2. No APT-initiated reboot. Rebooting a node is the reboot window's
//    decision and kured's; APT rebooting on its own schedule is an outage
//    outside the window, and on a single-server cluster an API outage.
//
// k3s and its container runtime are not APT packages, so nothing is held
// here: this policy installs nothing but security updates.
#clear Unattended-Upgrade::Allowed-Origins;
Unattended-Upgrade::Allowed-Origins {
	"${distro_id}:${distro_codename}-security";
};

Unattended-Upgrade::Automatic-Reboot "false";
`

// needrestartConf is the rendered 99-kubenest.conf.
//
// The entries ADD to needrestart's override map (`$nrconf{override_rc}{...} =
// 0`), they do not assign a new map to it: replacing the map would drop
// needrestart's own defaults — dbus, the display managers, systemd-logind —
// and start restarting services Ubuntu deliberately leaves alone. `0` is
// needrestart's "do not select this service for restart".
const needrestartConf = `# The KubeNest needrestart policy. Written by the installer (and, for
# existing 1.1 clusters, by the 1.2 upgrade host step); do not edit it, it is
# overwritten on every run.
#
# needrestart inspects every running service after a package update and offers
# to restart the ones whose libraries changed. k3s is not an APT package, but a
# k3s restart outside a reboot window is an API outage on a single-server
# cluster, so both units are on the ignore list. The list ships regardless of
# what probe P6 (9.1: can needrestart restart k3s after an unattended upgrade?)
# observed: the ignore list is the plan's stated fallback.
$nrconf{override_rc}{qr(^k3s\.service$)} = 0;
$nrconf{override_rc}{qr(^k3s-agent\.service$)} = 0;
`

// DropIn renders the apt.conf.d drop-in. Exported because the e2e gate
// compares the file a real host holds with what this build writes.
func DropIn() []byte { return []byte(aptDropIn) }

// NeedrestartConf renders the needrestart drop-in.
func NeedrestartConf() []byte { return []byte(needrestartConf) }

// Apply writes both policy files on one host, idempotently: it reads each file
// back and writes only what differs, so a second apply (an install re-run, or
// the 1.2 upgrade host step on an already-installed cluster, T7.3) changes
// nothing.
//
// Both files are written on every host of every role: a server runs
// k3s.service and an agent runs k3s-agent.service, the APT policy is the same
// on both, and the ignore list names both units so a host whose role changes
// (an agent promoted to a server) is already covered.
func Apply(ctx context.Context, r k3s.Runner) error {
	for _, f := range []struct{ path, content string }{
		{APTConfPath, aptDropIn},
		{NeedrestartConfPath, needrestartConf},
	} {
		if err := applyFile(ctx, r, f.path, f.content); err != nil {
			return err
		}
	}
	return nil
}

// applyFile writes content to path when, and only when, the file there
// differs. The comparison is the file's own bytes: a policy that is already
// what we would write is left alone, timestamp and all.
func applyFile(ctx context.Context, r k3s.Runner, file, content string) error {
	current, err := readFile(ctx, r, file)
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}
	if current == content {
		return nil
	}
	// The parent directory is created when it is missing: needrestart's
	// conf.d exists only while its package is installed, and a host that gets
	// needrestart later must find its ignore list already in place. install -d
	// is a no-op on a directory that exists.
	dir := path.Dir(file)
	if res, err := r.Run(ctx, "sudo -n install -d -m 0755 "+dir); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	} else if res.ExitCode != 0 {
		return fmt.Errorf("create %s: exit %d: %s", dir, res.ExitCode, firstLine(res.Stderr))
	}

	tmp := file + ".tmp"
	// k3s.WriteManifest's discipline, including the reason the content travels
	// over stdin: a command string is the argv of the shell sshd spawns on the
	// target host. These files hold no secret today, and this is what keeps the
	// next thing written here from having to remember that.
	res, err := r.RunInput(ctx,
		"sudo -n install -m "+confMode+" /dev/stdin "+tmp+
			" && sudo -n mv -f "+tmp+" "+file+
			" || { sudo -n rm -f "+tmp+"; false; }",
		strings.NewReader(content))
	if err != nil {
		return fmt.Errorf("write %s: %w", file, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("write %s: exit %d: %s", file, res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

// readFile returns the file's current content, or nothing when it is not
// there. `|| true` is what turns absence into a value rather than an error:
// the caller's decision is content equality, and a missing file is simply not
// equal to what is about to be written.
func readFile(ctx context.Context, r k3s.Runner, path string) (string, error) {
	res, err := r.Run(ctx, "sudo -n cat "+path+" 2>/dev/null || true")
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	return res.Stdout, nil
}

// Effective is the APT update policy a host actually runs, read back from the
// host's own configuration.
type Effective struct {
	// AllowedOrigins is the effective Unattended-Upgrade::Allowed-Origins
	// list, exactly as the host's configuration spells it: the
	// ${distro_id}/${distro_codename} placeholders are what appears in the
	// file and in `apt-config dump`, and unattended-upgrades substitutes them
	// at run time.
	AllowedOrigins []string
	// AutomaticReboot is whether APT is allowed to reboot this host by
	// itself.
	AutomaticReboot bool
}

// ReadEffective reads the host's effective APT configuration.
//
// The read is `apt-config dump` and not the policy file, deliberately: a read
// of the file the installer wrote would only prove that the write happened. A
// file that sorts after ours can clear the list again or turn the reboot back
// on, and only the effective configuration can see that. For the same reason
// an unreadable configuration is an error the caller must report rather than
// a quiet "no finding": the installer may not claim a host is pinned to a
// policy it cannot read.
func ReadEffective(ctx context.Context, r k3s.Runner) (Effective, error) {
	res, err := r.Run(ctx, aptConfigDump)
	if err != nil {
		return Effective{}, fmt.Errorf("%s: %w", aptConfigDump, err)
	}
	if res.ExitCode != 0 {
		return Effective{}, fmt.Errorf("%s: exit %d: %s", aptConfigDump, res.ExitCode, firstLine(res.Stderr))
	}
	return parseEffective(res.Stdout), nil
}

// parseEffective reads an `apt-config dump` listing. Values are printed
// quoted and terminated with `;`; a list is a parent node with an empty value
// plus one child per entry (the child's tag ends in `::`).
func parseEffective(dump string) Effective {
	var e Effective
	for _, line := range strings.Split(dump, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		switch {
		case key == allowedOriginsKey:
			// A list written as one comma-separated value rather than a
			// block: the block form leaves this empty.
			for _, origin := range strings.Split(unquote(value), ",") {
				if origin = strings.TrimSpace(origin); origin != "" {
					e.AllowedOrigins = append(e.AllowedOrigins, origin)
				}
			}
		case strings.HasPrefix(key, allowedOriginsKey+"::"):
			if origin := unquote(value); origin != "" {
				e.AllowedOrigins = append(e.AllowedOrigins, origin)
			}
		case key == automaticRebootKey:
			e.AutomaticReboot = strings.EqualFold(unquote(value), "true")
		}
	}
	return e
}

// unquote strips the quotes and the trailing `;` apt-config prints around a
// value.
func unquote(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimSuffix(value, ";")
	return strings.Trim(strings.TrimSpace(value), `"`)
}

// NonCompliance describes why the effective policy is not the one the
// installer writes, or returns "" when it is.
//
// It is deliberately about the EFFECT: any origin that is not a security
// pocket is a finding (the ESM security pockets qualify, the release pocket
// and -updates do not), and so is an empty origin list — a host that installs
// nothing at all is not "pinned to security updates", it is pinned to no
// updates, which is the silent failure this check exists to catch.
func (e Effective) NonCompliance() string {
	if e.AutomaticReboot {
		return `the effective APT configuration sets Unattended-Upgrade::Automatic-Reboot "true", so APT may reboot this node outside a reboot window`
	}
	if len(e.AllowedOrigins) == 0 {
		return "the effective APT configuration allows no unattended-upgrade origins, so security updates would never install"
	}
	var notSecurity []string
	for _, origin := range e.AllowedOrigins {
		if !strings.HasSuffix(origin, "-security") {
			notSecurity = append(notSecurity, origin)
		}
	}
	if len(notSecurity) > 0 {
		return fmt.Sprintf("the effective APT configuration also installs from %s, which is not a security pocket",
			strings.Join(notSecurity, ", "))
	}
	return ""
}

// firstLine is the part of a command's stderr worth putting in an error.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
