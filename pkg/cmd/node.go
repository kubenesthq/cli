package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewNodeCommand groups the node-lifecycle verbs (PLAN 7.3). Reboot is the
// first of them; add, remove and replace (T5.2-T5.4) join this group, and the
// one registration line in root.go is shared by whoever lands here first.
//
// The verbs live under `node` rather than under `platform` because they act on
// ONE machine of an existing cluster, which the cluster's own record names
// (kn-t50) — not on the platform installed across it.
func NewNodeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Operate one node of a cluster",
	}
	cmd.AddCommand(newNodeRebootCommand())
	return cmd
}

// newNodeRebootCommand is `kubenest node reboot`.
func newNodeRebootCommand() *cobra.Command {
	var f NodeRebootFlags
	cmd := &cobra.Command{
		Use:   "reboot --cluster C --node N [--confirm] [--wait | --now] [--k3s-only]",
		Short: "Reboot one node inside the maintenance window, or restart its k3s with --k3s-only",
		Long: `Reboot one node of a cluster, or restart its k3s service.

This is the manual path for servers, which never reboot by themselves in 1.2:
their reboots go through this command, inside the cluster's maintenance window.

On a multi-node cluster the node is cordoned and then drained within the
bundle's limits.timeouts.node-drain, respecting PodDisruptionBudgets; a pod is
never force-deleted. After the node returns Ready it is uncordoned. On a
single-server cluster there is NO drain — the control plane and every workload
live on the machine that is going down, so there is nowhere for the pods to go
— and this command says so instead of pretending it drained. The wait then
happens from OUTSIDE, over SSH, because nothing inside the cluster survives to
observe the reboot: SSH reachable, then k3s active, then the cluster's API
answering with the node Ready again, each within
limits.timeouts.node-reboot, and each reported as it is observed.

--k3s-only restarts k3s (or k3s-agent) instead of rebooting the host. It is the
supported way to renew k3s leaf certificates on a node that is not rebooting
for patches: k3s rotates them on startup when they are inside the renewal
window. The host is not rebooted, and that is asserted rather than assumed.

The node's address, SSH user and host-key fingerprint come from the cluster's
host inventory in its control-plane record, never from a local install journal,
so this works from any laptop. --ssh-user and --ssh-key only override how a
laptop AUTHENTICATES to that address. Before anything destructive the
fingerprint and the recorded Node UID are re-checked against the cluster, and a
mismatch is refused rather than guessed at.

The window rule is the cluster's: outside its window this refuses and names the
next opening in local time and UTC. --wait holds until the window opens, holding
nothing while it waits, and only then takes the operation record and re-checks
every gate. --now bypasses ONLY the window: quorum, storage, recovery-point and
kured-interlock checks still run, because asking to act now is not asking to
act on a cluster that cannot take it.

While it works, this command holds the cluster's operation record (one
disruptive operation at a time, resumable with --resume) AND kured's own lock,
so kured and this command can never take a node down at the same time.`,
		Example: `  # Renew this server's k3s certificates without rebooting the host:
  kubenest node reboot --cluster prod-1 --node prod-1-srv-1 --k3s-only --confirm

  # Reboot one node inside the window, waiting for it to open:
  kubenest node reboot --cluster prod-1 --node 10.0.3.7 --confirm --wait`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.Cluster == "" {
				return fmt.Errorf("--cluster is required")
			}
			if f.Node == "" {
				return fmt.Errorf("--node is required: a cluster node name, an SSH address, or the host ID the cluster's inventory records")
			}
			if err := f.validate(); err != nil {
				return err
			}
			// The inventory and the window both live in the control plane, so
			// the CLI's floor is checked before either is read.
			client, err := controlPlaneClientChecked(cmd.Context(), "node reboot")
			if err != nil {
				return err
			}
			return runNodeReboot(cmd.Context(), cmd.OutOrStdout(), client, f)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&f.Cluster, "cluster", "", "cluster whose node to act on (required)")
	fs.StringVar(&f.Node, "node", "", "the node: a cluster node name, an SSH address, or a host ID from the cluster's host inventory (required)")
	fs.BoolVar(&f.Confirm, "confirm", false, "confirm the action. Without it the plan is printed and nothing on the cluster or the host is changed")
	fs.BoolVar(&f.Wait, "wait", false, "hold until the maintenance window opens — holding nothing while it waits — then take the operation record and re-check every gate (requires --confirm)")
	fs.BoolVar(&f.Now, "now", false, "act immediately, bypassing ONLY the maintenance window; quorum, storage, recovery-point and kured-interlock checks still run")
	fs.BoolVar(&f.K3sOnly, "k3s-only", false, "restart k3s (or k3s-agent) instead of rebooting the host: renews this node's k3s leaf certificates and never reboots")
	fs.StringVar(&f.SSHUser, "ssh-user", "", "SSH user to reach the node with; the host inventory's user is used by default")
	fs.StringVar(&f.SSHKey, "ssh-key", "", "SSH private key file; defaults to ssh-agent or ~/.ssh/config")
	fs.StringVar(&f.Resume, "resume", "", "continue an interrupted reboot by operation id, as reported when it stopped")
	return cmd
}
