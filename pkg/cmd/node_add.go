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

// NodeAddFlags is the flag surface of `kubenest node add` (PLAN 7.3).
//
// It is exported because the e2e gate drives the verb through the same entry
// point the command does: a gate that rebuilt the flag surface of its own would
// accept a command the CLI does not have.
type NodeAddFlags struct {
	// Cluster is the cluster's name, as recorded at install.
	Cluster string
	// Agent is the machine to add: its SSH address, or a host ID the
	// cluster's inventory already knows.
	Agent string
	// StorageDevice is the stable /dev/disk/by-id/... path of a blank device
	// to create kubenest-vg on. Absent means Option 1 — the volume group
	// already exists, exactly as `platform install` treats it.
	StorageDevice string
	// Resume continues an interrupted add by operation id.
	Resume string
	// Wait holds until the maintenance window opens, holding nothing while it
	// waits.
	Wait bool
	// Now bypasses the maintenance window and NOTHING else.
	Now bool
	// SSHUser and SSHKey override how this laptop AUTHENTICATES to the machine
	// the inventory records. They never decide WHICH machine it is.
	SSHUser string
	SSHKey  string
}

func (f NodeAddFlags) validate() error {
	if f.Wait && f.Now {
		return fmt.Errorf("--wait and --now are mutually exclusive: --wait holds for the maintenance window, --now acts immediately and bypasses it")
	}
	return nil
}

// newNodeAddCommand is `kubenest node add`.
func newNodeAddCommand() *cobra.Command {
	var f NodeAddFlags
	cmd := &cobra.Command{
		Use:   "add --cluster C --agent HOST [--storage-device DEV] [--resume ID] [--wait | --now]",
		Short: "Add one agent to a cluster, inside the maintenance window",
		Long: `Add a machine to an existing cluster as an agent.

The machine joins at the bundle's pinned k3s version, through a server from the
cluster's HOST INVENTORY that is actually Ready — never simply the first server
installed, and never a server named on this command line. Before anything is
written to the machine it passes the same pre-flight checks the installer runs,
including the bundle's tested Ubuntu release; an unsupported release is refused
before the host is touched.

--storage-device names a blank device to build kubenest-vg on, exactly as
` + "`platform install`" + ` treats it. Without it the volume group must already
exist, and this command verifies it rather than creating anything.

The host is written into the cluster's inventory as joining BEFORE it is
touched, and marked active only once it is Ready and its volume group exists.
Until then it carries kubenest.io/auto-reboot=false, so kured cannot reboot a
node that joined but is not yet recorded; the hold is lifted last.

While it works this command holds the cluster's operation record (one
disruptive operation at a time, resumable with --resume) AND kured's own lock,
so kured and this command can never take a node down at the same time. A node
operation that is interrupted resumes from its journal and its record: work
that already happened is not repeated.

In bundle 1.2 this acts on AGENTS only: a single-server cluster's only server
is recovered by S6, and the ha tier's node operations arrive with its promotion.`,
		Example: `  # Add a machine with a blank device to build the volume group on:
  kubenest node add --cluster prod-1 --agent 10.0.3.9 --storage-device /dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive3

  # Add a machine whose volume group already exists, inside the window:
  kubenest node add --cluster prod-1 --agent 10.0.3.10 --wait`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.Cluster == "" {
				return fmt.Errorf("--cluster is required")
			}
			if f.Agent == "" {
				return fmt.Errorf("--agent is required: the SSH address of the machine to add, or a host ID from the cluster's inventory")
			}
			if err := f.validate(); err != nil {
				return err
			}
			// The inventory, the window and the bundle all live in the control
			// plane, so the CLI's floor is checked before any of them is read.
			// `node add` needs the control plane exactly as `node reboot`
			// does; the floor is checked here because the list of command
			// paths that need it is another bead's file.
			client, err := controlPlaneClient()
			if err != nil {
				return err
			}
			if err := requireControlPlane(cmd.Context(), client); err != nil {
				return err
			}
			return runNodeAdd(cmd.Context(), cmd.OutOrStdout(), client, f)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&f.Cluster, "cluster", "", "cluster to add the node to (required)")
	fs.StringVar(&f.Agent, "agent", "", "the machine to add: its SSH address, or a host ID from the cluster's inventory (required)")
	fs.StringVar(&f.StorageDevice, "storage-device", "", "a blank device to create kubenest-vg on, as /dev/disk/by-id/...; omit when the volume group already exists")
	fs.StringVar(&f.Resume, "resume", "", "continue an interrupted add by operation id, as reported when it stopped")
	fs.BoolVar(&f.Wait, "wait", false, "hold until the maintenance window opens — holding nothing while it waits — then take the locks and re-check every gate")
	fs.BoolVar(&f.Now, "now", false, "act immediately, bypassing ONLY the maintenance window; the interlock, storage and pre-flight checks still run")
	fs.StringVar(&f.SSHUser, "ssh-user", "", "SSH user to reach the new machine with; your own SSH configuration is used by default")
	fs.StringVar(&f.SSHKey, "ssh-key", "", "SSH private key file; defaults to ssh-agent or ~/.ssh/config")
	return cmd
}

// runNodeAdd is the entry point the command and the e2e gate both call.
func runNodeAdd(ctx context.Context, out io.Writer, client *api.Client, f NodeAddFlags) error {
	session, err := prepareNodeSession(ctx, out, client, f.Cluster, string(operation.KindNodeAdd), f.Agent, f.StorageDevice, f.SSHUser, f.SSHKey)
	if err != nil {
		return err
	}
	add := &node.Add{Session: session, Opts: node.AddOptions{
		Agent:         f.Agent,
		StorageDevice: f.StorageDevice,
		Resume:        f.Resume,
		Wait:          f.Wait,
		Now:           f.Now,
	}}

	// AN INTERRUPT IS NOT A FAILURE. SIGINT/SIGTERM cancels the run and leaves
	// the operation record STOPPED rather than terminal-failed, so a second
	// laptop can continue it: a terminal record can only be restarted, and
	// restarting a join that half-happened is what the record exists to avoid.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(out, "Adding agent %s to cluster %s (bundle %s).\n", f.Agent, f.Cluster, session.Bundle.Bundle)
	result, runErr := stages.Execute(ctx, add, node.PlanAdd(add))
	add.Finish(context.WithoutCancel(ctx), runErr, ctx.Err() != nil)
	if runErr != nil {
		return runErr
	}
	fmt.Fprintf(out, "\nAdded %s to cluster %s in %s: it is recorded in the cluster's inventory and is included by the next upgrade.\n",
		f.Agent, f.Cluster, result.Elapsed.Round(time.Second))
	return nil
}
