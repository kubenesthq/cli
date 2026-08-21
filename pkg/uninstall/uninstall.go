// Package uninstall removes the platform from the hosts it was installed on.
//
// Enterprise buyers ask about the exit before they commit to the entry, so
// the exit is documented and tested rather than improvised. What that means
// concretely:
//
//	Uninstall never destroys data by default. Persistent volumes and their
//	contents survive. --destroy-data removes them.
//
//	Even with --destroy-data, the kubenest-vg volume group is removed only if
//	the INSTALLER created it, recorded in the journal at install time. A
//	volume group the customer created is never removed, on either path — the
//	installer never touched their block devices going in and does not touch
//	them coming out.
//
//	Ownership that cannot be established is treated as the customer's. A
//	missing journal means "do not know", and "do not know" must never resolve
//	to "destroy": the failure mode of guessing wrong here is a customer's
//	data, and there is no undo.
package uninstall

import (
	"context"
	"fmt"
	"io"
	"strings"

	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/storage"
)

// Role is what the node was installed as; it decides which k3s uninstall
// script exists on it.
type Role string

const (
	RoleServer Role = "server"
	RoleAgent  Role = "agent"
)

// Node is one host to clean.
type Node struct {
	Address string
	Role    Role
	Runner  k3s.Runner
}

// Options is one uninstall run.
type Options struct {
	Nodes []Node
	// DestroyData removes persistent volumes. Without it they survive.
	DestroyData bool
	// Ownership is the journal's record of who created kubenest-vg. Empty
	// means unknown, which is treated as the customer's.
	Ownership storage.Ownership
	// Device is the block device the installer used, if it created the
	// volume group.
	Device string
	// Out receives a line per action, so an operator can see exactly what
	// was removed and what was deliberately left.
	Out io.Writer
}

// Run removes the platform from every node.
//
// It is best-effort per node and reports everything that went wrong at the
// end rather than stopping at the first problem: a half-uninstalled fleet is
// worse than a fully attempted one, and the operator needs the whole picture
// to finish by hand if something resists.
func Run(ctx context.Context, opts Options) error {
	var problems []string

	// Agents first: they are the ones that lose their server.
	for _, node := range ordered(opts.Nodes) {
		logf(opts.Out, "%s: removing k3s", node.Address)
		if err := removeK3s(ctx, node); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", node.Address, err))
			continue
		}
		if err := removeData(ctx, opts, node); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", node.Address, err))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("uninstall did not complete on every node:\n  %s\n\nthe nodes above still carry platform state; the others are clean",
			strings.Join(problems, "\n  "))
	}
	return nil
}

// ordered puts agents before servers, so a node does not lose its API server
// while it is still being drained of platform state.
func ordered(nodes []Node) []Node {
	out := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Role == RoleAgent {
			out = append(out, n)
		}
	}
	for _, n := range nodes {
		if n.Role != RoleAgent {
			out = append(out, n)
		}
	}
	return out
}

// k3s's own uninstall scripts remove the binary, the systemd units,
// containerd's state and /var/lib/rancher/k3s — including the auto-deploy
// directory, which is every manifest and HelmChart the installer placed.
// Using them rather than a hand-rolled teardown is deliberate: they are
// maintained with k3s and they know what k3s wrote.
const (
	serverUninstall = "/usr/local/bin/k3s-uninstall.sh"
	agentUninstall  = "/usr/local/bin/k3s-agent-uninstall.sh"
)

func removeK3s(ctx context.Context, node Node) error {
	script := serverUninstall
	if node.Role == RoleAgent {
		script = agentUninstall
	}
	// Either script may be absent: on a node where the install failed before
	// stage 3, or on a re-run. That is a clean node, not an error — uninstall
	// has to be re-runnable.
	cmd := fmt.Sprintf("if [ -x %s ]; then sudo -n %s; else echo 'no k3s installation'; fi", script, script)
	res, err := node.Runner.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("removing k3s: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("removing k3s: exit %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	// The join token, if an interrupted install left one staged.
	_, _ = node.Runner.Run(ctx, "sudo -n rm -f /etc/rancher/kubenest-join-token")
	return nil
}

// removeData applies the data rules. Read them as a sequence of refusals:
// nothing is removed unless the operator asked AND the record says it is ours.
func removeData(ctx context.Context, opts Options, node Node) error {
	if !opts.DestroyData {
		logf(opts.Out, "%s: persistent volumes left in place (pass --destroy-data to remove them)", node.Address)
		return nil
	}

	volumes, err := platformVolumes(ctx, node)
	if err != nil {
		return err
	}
	for _, lv := range volumes {
		logf(opts.Out, "%s: removing volume %s/%s", node.Address, storage.VolumeGroup, lv)
		res, err := node.Runner.Run(ctx, fmt.Sprintf("sudo -n lvremove -y %s/%s", storage.VolumeGroup, shellArg(lv)))
		if err != nil {
			return fmt.Errorf("removing volume %s: %w", lv, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("removing volume %s: exit %d: %s", lv, res.ExitCode, firstLine(res.Stderr))
		}
	}

	if opts.Ownership != storage.InstallerCreated {
		reason := "you created it"
		if opts.Ownership == "" {
			reason = "there is no record of who created it, and an unknown owner is treated as you"
		}
		logf(opts.Out, "%s: volume group %s left in place — %s", node.Address, storage.VolumeGroup, reason)
		return nil
	}

	logf(opts.Out, "%s: removing volume group %s (the installer created it)", node.Address, storage.VolumeGroup)
	res, err := node.Runner.Run(ctx, "sudo -n vgremove -y "+storage.VolumeGroup)
	if err != nil {
		return fmt.Errorf("removing volume group: %w", err)
	}
	if res.ExitCode != 0 && !strings.Contains(res.Stderr, "not found") {
		return fmt.Errorf("removing volume group: exit %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	if opts.Device == "" {
		return nil
	}
	logf(opts.Out, "%s: releasing %s", node.Address, opts.Device)
	res, err = node.Runner.Run(ctx, "sudo -n pvremove -y "+shellArg(opts.Device))
	if err != nil {
		return fmt.Errorf("releasing %s: %w", opts.Device, err)
	}
	if res.ExitCode != 0 && !strings.Contains(res.Stderr, "not found") {
		return fmt.Errorf("releasing %s: exit %d: %s", opts.Device, res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

// platformVolumes lists the logical volumes in kubenest-vg. OpenEBS Local PV
// LVM names each one after the PersistentVolume it backs (pvc-<uuid>), so
// this is the set of persistent volumes on this node — and nothing else in
// the volume group is touched even with --destroy-data, because a customer
// may keep their own logical volumes in a volume group they created.
func platformVolumes(ctx context.Context, node Node) ([]string, error) {
	res, err := node.Runner.Run(ctx,
		"sudo -n lvs --noheadings -o lv_name "+storage.VolumeGroup+" 2>/dev/null || true")
	if err != nil {
		return nil, fmt.Errorf("listing volumes: %w", err)
	}
	var out []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		name := strings.TrimSpace(line)
		if strings.HasPrefix(name, "pvc-") {
			out = append(out, name)
		}
	}
	return out, nil
}

// shellArg refuses anything that is not a plain device or volume name rather
// than quoting it. These strings reach a command that removes data.
func shellArg(s string) string {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '/', r == ':', r == '+':
		default:
			return "''"
		}
	}
	return s
}

func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, format+"\n", args...)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
