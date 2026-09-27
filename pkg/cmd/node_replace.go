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
)

// NodeReplaceFlags is the flag surface of `kubenest node replace` (PLAN 7.3).
type NodeReplaceFlags struct {
	// Cluster is the cluster's name, as recorded at install.
	Cluster string
	// Node names the machine being replaced: the inventory's host ID, its SSH
	// address, or the cluster's Node name.
	Node string
	// With names the machine that replaces it.
	With string
	// ConfirmIsolated is the operator's affirmative answer that a machine which
	// does not answer is powered off or isolated at the provider, and stays that
	// way until it is wiped.
	ConfirmIsolated bool
	// StorageDevice is a blank device to create kubenest-vg on on the machine
	// that joins. Absent means the volume group already exists there.
	StorageDevice string
	// Resume continues an interrupted replace by operation id.
	Resume string
	// Wait holds until the maintenance window opens, holding nothing while it
	// waits.
	Wait bool
	// Now bypasses the maintenance window and NOTHING else.
	Now bool
	// SSHUser and SSHKey override how this laptop AUTHENTICATES to the machines
	// the inventory records. They never decide WHICH machine one is.
	SSHUser string
	SSHKey  string
}

func (f NodeReplaceFlags) validate() error {
	if f.Wait && f.Now {
		return fmt.Errorf("--wait and --now are mutually exclusive: --wait holds for the maintenance window, --now acts immediately and bypasses it")
	}
	return nil
}

// newNodeReplaceCommand is `kubenest node replace`.
func newNodeReplaceCommand() *cobra.Command {
	var f NodeReplaceFlags
	cmd := &cobra.Command{
		Use:   "replace --cluster C --node N --with HOST [--confirm-isolated] [--storage-device DEV] [--resume ID] [--wait | --now]",
		Short: "Replace one agent: join the replacement, then take the machine it replaces out",
		Long: `Replace one agent of a cluster: a new machine joins, and the machine it
replaces leaves.

THE ORDER IS THE SAFETY PROPERTY, and it follows from the machine rather than
from a flag. If the machine answers SSH and the cluster reports its Node Ready,
the replacement is added FIRST and the old machine is removed afterwards, so the
cluster never loses capacity for the workload set. If it does not answer, it is
removed first and the replacement joins afterwards — etcd's own order, and the
only order that works when the old machine may come back.

A machine that does not answer is where this command stops deciding for you. It
refuses until you confirm, at the provider, that the machine is POWERED OFF or
ISOLATED, naming it by host ID and address, and that it stays that way until it
is wiped. --confirm-isolated records that answer; nothing here fends anything
off, because deleting a Node object removes it from the cluster and neither
stops the machine coming back nor revokes the credentials already on that host.
A returning, unwiped machine is not refused automatically — the inventory marks
the replaced host removed with its host key as bookkeeping, and that is all.

LOCAL VOLUMES ARE THE HARD PART, and this command says so rather than making a
second stranded set. OpenEBS LVM volumes live on one node's disk, so a REACHABLE
machine that holds bound local volumes is refused BEFORE the replacement is
added, with the migration to run instead: back the data up, restore each
workload's claims elsewhere (one restore command per workload, with every claim
of that workload in it), then replace. A machine that is gone holds its data
beyond the cluster's reach: the replacement joins and the command then prints
one restore command per affected workload, to be run now that the volumes have a
live node to land on. Anything written on the dead machine after its last
backup's capture began is gone.

A machine that has joined goes through the same stages node add runs — the
install's own pre-flight, the bundle's pinned k3s version, the volume group, the
no-automatic-reboot hold until the inventory entry says it is active — and the
machine that leaves goes through the stages node remove runs: it is held out of
kured's pool, written down as removing, cordoned, drained within the bundle's
node-drain timeout and the PodDisruptionBudgets, has k3s uninstalled if it can
be reached, has its Node object deleted, and is recorded removed with its host
ID and host key kept.

A server is refused: a single-server cluster's only server is recovered by S6
(kubenest platform restore from the recovery kit and an off-host backup), and
the ha tier's server operations arrive with its promotion.

While it works this command holds ONE operation record — the same kind of lock a
node add holds, so a replace cannot race one — and kured's own lock, taken
before the first disruptive step. An interrupted replace resumes with --resume,
and a resume continues the order it started in, because the order is part of the
operation's immutable request.`,
		Example: `  # A machine that is gone, after confirming its isolation at the provider:
  kubenest node replace --cluster prod-1 --node prod-1-agt-2 --with 10.0.3.9 --confirm-isolated --now

  # A machine that answers, with a blank device to build the volume group on:
  kubenest node replace --cluster prod-1 --node 10.0.3.8 --with 10.0.3.9 --storage-device /dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive3 --wait

  # Refused with the migration to run instead, because the machine holds local volumes:
  kubenest node replace --cluster prod-1 --node 10.0.3.8 --with 10.0.3.9`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.Cluster == "" {
				return fmt.Errorf("--cluster is required")
			}
			if f.Node == "" {
				return fmt.Errorf("--node is required: the machine being replaced, as an inventory host ID, an SSH address, or the cluster's Node name")
			}
			if f.With == "" {
				return fmt.Errorf("--with is required: the machine that replaces it, as an SSH address or a host ID the cluster's inventory already knows")
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
			return runNodeReplace(cmd.Context(), cmd.OutOrStdout(), client, f)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&f.Cluster, "cluster", "", "cluster whose node to replace (required)")
	fs.StringVar(&f.Node, "node", "", "the machine being replaced: an inventory host ID, an SSH address, or the cluster's Node name (required)")
	fs.StringVar(&f.With, "with", "", "the machine that replaces it: its SSH address, or a host ID the inventory already knows (required)")
	fs.BoolVar(&f.ConfirmIsolated, "confirm-isolated", false, "confirm that a machine which does not answer is powered off or isolated at the provider and stays that way until it is wiped. Required by the order that removes first, and recorded as your answer rather than assumed")
	fs.StringVar(&f.StorageDevice, "storage-device", "", "a blank device on the machine that joins, to create kubenest-vg on, as /dev/disk/by-id/...; omit when the volume group already exists there")
	fs.StringVar(&f.Resume, "resume", "", "continue an interrupted replace by operation id, as reported when it stopped")
	fs.BoolVar(&f.Wait, "wait", false, "hold until the maintenance window opens — holding nothing while it waits — then take the locks and re-check every gate")
	fs.BoolVar(&f.Now, "now", false, "act immediately, bypassing ONLY the maintenance window; the interlock, volume and role checks still run")
	fs.StringVar(&f.SSHUser, "ssh-user", "", "SSH user to reach the machines with; the host inventory's user is used by default")
	fs.StringVar(&f.SSHKey, "ssh-key", "", "SSH private key file; defaults to ssh-agent or ~/.ssh/config")
	return cmd
}

// runNodeReplace is the entry point the command and the e2e gate both call.
func runNodeReplace(ctx context.Context, out io.Writer, client *api.Client, f NodeReplaceFlags) error {
	// The journal's identity carries the machine being replaced and the blank
	// device: a resume that names a different machine is a different operation,
	// and the record refuses it by name.
	session, err := prepareNodeSession(ctx, out, client, f.Cluster, string(operation.KindNodeReplace), f.Node, f.StorageDevice, f.SSHUser, f.SSHKey)
	if err != nil {
		return err
	}
	replace := node.NewReplace(session, node.ReplaceOptions{
		Node:            f.Node,
		With:            f.With,
		ConfirmIsolated: f.ConfirmIsolated,
		StorageDevice:   f.StorageDevice,
		Resume:          f.Resume,
		Wait:            f.Wait,
		Now:             f.Now,
	})

	// An interrupt stops the operation rather than failing it, so a second
	// laptop can continue it (see runNodeAdd).
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(out, "Replacing node %s of cluster %s with %s (bundle %s).\n", f.Node, f.Cluster, f.With, session.Bundle.Bundle)
	result, runErr := node.RunReplace(ctx, replace)
	replace.Finish(context.WithoutCancel(ctx), runErr, ctx.Err() != nil)
	if runErr != nil {
		return runErr
	}
	fmt.Fprintf(out, "\nReplaced %s with %s in %s: the cluster has the replacement.\n", f.Node, f.With, result.Elapsed.Round(time.Second))
	return nil
}
