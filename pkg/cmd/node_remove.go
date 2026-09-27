package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/node"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/stages"
)

// NodeRemoveFlags is the flag surface of `kubenest node remove` (PLAN 7.3.2).
type NodeRemoveFlags struct {
	// Cluster is the cluster's name, as recorded at install.
	Cluster string
	// Node names the machine: the inventory's host ID, its SSH address, or the
	// cluster's Node name.
	Node string
	// AbandonVolumes acknowledges the loss of every local volume the node
	// holds. It certifies nothing about backups.
	AbandonVolumes bool
	// Resume continues an interrupted remove by operation id.
	Resume string
	// TakeOver takes over a removal whose record still says its executor is
	// running, on the operator's assertion that the previous executor and its
	// outstanding actions have stopped (PLAN 7.2, kn-yzuv).
	TakeOver string
	// Confirm is that assertion, which --take-over requires: the CLI never
	// infers that the previous executor is gone.
	Confirm bool
	// Wait holds until the maintenance window opens, holding nothing while it
	// waits.
	Wait bool
	// Now bypasses the maintenance window and NOTHING else.
	Now bool
	// SSHUser and SSHKey override how this laptop AUTHENTICATES to the node.
	SSHUser string
	SSHKey  string
}

// recovery is the two recovery flags as pkg/operation's Recovery, where their
// rules live: mutually exclusive, and --take-over requires --confirm.
func (f NodeRemoveFlags) recovery() operation.Recovery {
	return operation.Recovery{Resume: f.Resume, TakeOver: f.TakeOver, Confirm: f.Confirm}
}

func (f NodeRemoveFlags) validate() error {
	if f.Wait && f.Now {
		return fmt.Errorf("--wait and --now are mutually exclusive: --wait holds for the maintenance window, --now acts immediately and bypasses it")
	}
	return f.recovery().Validate()
}

// newNodeRemoveCommand is `kubenest node remove`.
func newNodeRemoveCommand() *cobra.Command {
	var f NodeRemoveFlags
	cmd := &cobra.Command{
		Use:   "remove --cluster C --node N [--abandon-volumes] [--resume ID] [--wait | --now]",
		Short: "Remove one agent from a cluster, after settling what its local volumes hold",
		Long: `Remove one agent from a cluster.

This is the one node verb that can destroy data. Local PV LVM volumes live on
ONE node's disk, so removing the node removes their only copy. The command
therefore lists every claim bound to a local volume on that node and REFUSES
while any remains, printing the restore command for each affected workload —
with every stranded claim of a workload in one command, because restoring one
claim of a pod while another stays stranded leaves the pod unable to start.
k3s is never uninstalled before that disposition is settled.

--abandon-volumes proceeds anyway. It acknowledges that the data on those
claims is GONE, and it certifies NOTHING about backups; if the workload
matters, the lever is a more frequent Velero schedule for its namespace.

A node aimed at is resolved THROUGH THE CLUSTER'S INVENTORY, so a Node name and
a host ID cannot disagree about which machine this is. A server is refused:
S6 recovers a single-server cluster, and the ha tier's node operations arrive
with its promotion.

The removal cordons, then drains within limits.timeouts.node-drain and the
PodDisruptionBudgets (a budget that could never let the drain finish is refused
BEFORE the drain starts), then uninstalls k3s on the host if it can be reached,
deletes the Node object, and marks the host removed in the inventory — keeping
its host ID and host key so it is not re-added by accident.

While it works this command holds the cluster's operation record (one
disruptive operation at a time, resumable with --resume) AND kured's own lock.`,
		Example: `  # Refused, with the restore commands to run first (the usual case):
  kubenest node remove --cluster prod-1 --node 10.0.3.9

  # A node whose local volumes are meant to go:
  kubenest node remove --cluster prod-1 --node 10.0.3.9 --abandon-volumes --wait`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.Cluster == "" {
				return fmt.Errorf("--cluster is required")
			}
			if f.Node == "" {
				return fmt.Errorf("--node is required: the inventory's host ID or SSH address, or the cluster's Node name")
			}
			if err := f.validate(); err != nil {
				return err
			}
			// The inventory, the window and the bundle all live in the control
			// plane, so the CLI's floor is checked before any of them is read.
			client, err := controlPlaneClient()
			if err != nil {
				return err
			}
			if err := requireControlPlane(cmd.Context(), client); err != nil {
				return err
			}
			return runNodeRemove(cmd.Context(), cmd.OutOrStdout(), client, f)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&f.Cluster, "cluster", "", "cluster to remove the node from (required)")
	fs.StringVar(&f.Node, "node", "", "the node to remove: an inventory host ID, an SSH address, or the cluster's Node name (required)")
	fs.BoolVar(&f.AbandonVolumes, "abandon-volumes", false, "remove the node even though local volumes on it will be destroyed, acknowledging that the data on them is gone; certifies nothing about backups")
	fs.StringVar(&f.Resume, "resume", "", "continue an interrupted removal by operation id, as reported when it stopped")
	fs.StringVar(&f.TakeOver, "take-over", "", "take over a removal whose record still says its executor is running, on your assertion that the previous executor and its outstanding actions have stopped (requires --confirm)")
	fs.BoolVar(&f.Confirm, "confirm", false, "with --take-over: your assertion that the previous executor and its outstanding actions have stopped")
	fs.BoolVar(&f.Wait, "wait", false, "hold until the maintenance window opens — holding nothing while it waits — then take the locks and re-check every gate")
	fs.BoolVar(&f.Now, "now", false, "act immediately, bypassing ONLY the maintenance window; the interlock, volume and disruption-budget checks still run")
	fs.StringVar(&f.SSHUser, "ssh-user", "", "SSH user to reach the node with; the host inventory's user is used by default")
	fs.StringVar(&f.SSHKey, "ssh-key", "", "SSH private key file; defaults to ssh-agent or ~/.ssh/config")
	return cmd
}

// runNodeRemove is the entry point the command and the e2e gate both call.
func runNodeRemove(ctx context.Context, out io.Writer, client *api.Client, f NodeRemoveFlags) error {
	session, err := prepareNodeSession(ctx, out, client, f.Cluster, string(operation.KindNodeRemove), f.Node, "", f.SSHUser, f.SSHKey)
	if err != nil {
		return err
	}
	remove := &node.Remove{Session: session, Opts: node.RemoveOptions{
		Node:           f.Node,
		AbandonVolumes: f.AbandonVolumes,
		Resume:         f.Resume,
		TakeOver:       f.TakeOver,
		Confirm:        f.Confirm,
		Wait:           f.Wait,
		Now:            f.Now,
	}}

	// An interrupt stops the operation rather than failing it, so a second
	// laptop can continue it (see runNodeAdd).
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(out, "Removing a node from cluster %s (bundle %s).\n", f.Cluster, session.Bundle.Bundle)
	result, runErr := stages.Execute(ctx, remove, node.PlanRemove(remove))
	remove.Finish(context.WithoutCancel(ctx), runErr, ctx.Err() != nil)
	if runErr != nil {
		return runErr
	}
	fmt.Fprintf(out, "\nRemoved %s from cluster %s in %s: the cluster no longer has the node, and its inventory entry is kept as removed.\n",
		f.Node, f.Cluster, result.Elapsed.Round(time.Second))
	return nil
}
