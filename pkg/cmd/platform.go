package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// The platform command surface follows docs.kubenest.io/platform/install. The
// flag surface and its validation land here so the installer (kn-7k8) plugs
// into a settled interface; until it lands, the subcommands say so plainly
// instead of pretending.

// errNotYetImplemented marks skeleton commands. The exit code is non-zero so
// scripts cannot mistake a skeleton for a successful run.
func errNotYetImplemented(what string) error {
	return fmt.Errorf("%s is not available in this build of the CLI yet — the command surface is final, the implementation is landing. Watch https://github.com/kubenesthq/cli/releases", what)
}

// AnnotationUnavailable marks a registered command that is not built yet.
//
// It is the machine-readable half of the skeleton rule. The metadata generator
// (cmd/gen-command-metadata) reads it and reports the path as available: false,
// so the docs checker
// (kubenest-docs/scripts/check_examples_against_cli.py) refuses a runnable
// example of a stub by asking the command tree rather than by matching a
// sentence in the help text. errNotYetImplemented remains the runtime half;
// both are set together, or a path reports available and exits non-zero.
const AnnotationUnavailable = "kubenest.io/unavailable"

// InstallFlags is the flag surface of `kubenest platform install`, exactly as
// documented on the install page.
type InstallFlags struct {
	Bundle string
	Name   string
	// Org is only needed when the credential can see more than one
	// organization; registering a customer's cluster under the wrong one is
	// a support call, not a typo.
	Org           string
	Servers       []string
	Agents        []string
	HATier        string
	Profiles      []string
	SSHUser       string
	SSHKey        string
	StorageDevice string
	BackupTarget  string
	// ControlPlane installs the KubeNest control plane INTO this cluster and
	// registers the cluster to it: this is how the FIRST cluster of a fleet
	// is built, and it leaves the CLI logged in to https://api.<domain>.
	// Every later cluster is added with a plain `kubenest platform install`
	// against that login.
	ControlPlane bool
	// Domain is the name the control plane is served under: console at
	// app.<domain>, API at api.<domain>, and hub.<domain> for the agents of
	// every cluster added later. Defaults to <first --server
	// address>.sslip.io, which resolves to that address without any DNS
	// setup, so a first cluster needs nothing arranged in advance.
	Domain string
	// AdminEmail is the control plane's administrator account, which is
	// created during the install. Defaults to admin@<domain>.
	AdminEmail string
}

// controlPlaneDomain is the domain this install serves the control plane
// under: the value given with --domain, or one derived from the first
// --server's address.
func (f InstallFlags) controlPlaneDomain() string {
	if f.Domain != "" {
		return f.Domain
	}
	if len(f.Servers) == 0 {
		return ""
	}
	return f.Servers[0] + ".sslip.io"
}

// controlPlaneAdminEmail is the control plane's administrator account: the
// value given with --admin-email, or admin@<domain>.
func (f InstallFlags) controlPlaneAdminEmail(domain string) string {
	if f.AdminEmail != "" {
		return f.AdminEmail
	}
	return "admin@" + domain
}

// Validate applies the checks that need no manifest and no network: flag
// shape only. Everything deeper (profile names against the bundle, node
// counts against limits) is preflight's job and needs the manifest.
func (f *InstallFlags) Validate() error {
	if f.Bundle == "" {
		return fmt.Errorf("--bundle is required: the bundle version pins every component (see the Bundle contents page)")
	}
	if f.Name == "" {
		return fmt.Errorf("--name is required: the cluster is recorded under this name")
	}
	if f.ControlPlane && f.Org != "" {
		return fmt.Errorf("--org does not apply to --control-plane: the control plane being installed has one organization, and this cluster registers into it")
	}
	if f.Domain != "" && !f.ControlPlane {
		return fmt.Errorf("--domain only means anything with --control-plane: a cluster added to a fleet is served by the domain of the control plane it registers with")
	}
	if f.AdminEmail != "" && !f.ControlPlane {
		return fmt.Errorf("--admin-email only means anything with --control-plane: the administrator account belongs to the control plane, not to this cluster")
	}
	if len(f.Servers) == 0 {
		return fmt.Errorf("at least one --server is required")
	}
	switch f.HATier {
	case "single-server", "ha":
	case "":
		return fmt.Errorf("--ha is required and permanent for the cluster: choose single-server or ha deliberately (see \"Choose your tiers before you run\")")
	default:
		return fmt.Errorf("--ha %q is not a tier: the tiers are single-server and ha", f.HATier)
	}
	if f.HATier == "ha" && len(f.Servers) < 3 {
		return fmt.Errorf("--ha ha needs three control-plane nodes, got %d --server", len(f.Servers))
	}
	if f.HATier == "single-server" && len(f.Servers) > 1 {
		return fmt.Errorf("--ha single-server takes exactly one --server, got %d", len(f.Servers))
	}
	return nil
}

// NewPlatformCommand groups the platform lifecycle: install, uninstall, upgrade.
func NewPlatformCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "platform",
		Short: "Install, upgrade or remove the KubeNest Platform on your hosts",
	}
	cmd.AddCommand(
		newPlatformInstallCommand(),
		newPlatformUninstallCommand(),
		newPlatformUpgradeCommand(),
		newPlatformRollbackCommand(),
		newPlatformRestoreCommand(),
		newPlatformDiffCommand(),
	)
	return cmd
}

func newPlatformInstallCommand() *cobra.Command {
	var f InstallFlags

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the platform bundle onto Ubuntu hosts you supply",
		Long: `Install the complete platform bundle — k3s, ingress, certificates, storage,
backup, upgrade orchestration and OS patching — onto Ubuntu 24.04 hosts over
SSH, as one versioned unit.

Preflight checks everything before the first byte is written to any machine.
SSH keys come from --ssh-key, ssh-agent or ~/.ssh/config and never leave this
machine.

With --control-plane this install also puts the KubeNest control plane on the
first cluster: the console at https://app.<domain>, the API at
https://api.<domain>, an administrator account whose generated password is
printed once and stored on the cluster, and a CLI login on this machine. The
cluster is registered to that control plane. --domain defaults to
<first --server address>.sslip.io, which is public DNS answering with that
address, so nothing has to be set up in advance, and --admin-email defaults to
admin@<domain>.

Every later cluster is added with a plain install against the control plane
this machine is logged in to; it needs no control plane of its own.`,
		Example: `  # The first cluster: the platform, and the KubeNest control plane.
  kubenest platform install \
    --control-plane \
    --bundle 1.1 \
    --name prod-1 \
    --server 10.0.1.10 \
    --ha single-server \
    --ssh-user ubuntu \
    --ssh-key ~/.ssh/id_ed25519 \
    --storage-device /dev/nvme1n1

  # Every later cluster: registered with the control plane you are logged in to.
  kubenest platform install \
    --bundle 1.1 \
    --name prod-2 \
    --server 10.0.2.10 \
    --ha single-server \
    --ssh-user ubuntu \
    --ssh-key ~/.ssh/id_ed25519 \
    --storage-device /dev/nvme1n1`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := f.Validate(); err != nil {
				return err
			}
			return runInstall(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}

	fs := cmd.Flags()
	fs.StringVar(&f.Bundle, "bundle", "", "platform bundle version to install (required)")
	fs.StringVar(&f.Name, "name", "", "cluster name, recorded against the control plane (required)")
	fs.StringVar(&f.Org, "org", "", "organization slug or id (only needed when your credential can see more than one)")
	fs.BoolVar(&f.ControlPlane, "control-plane", false, "install the KubeNest control plane into this cluster and register the cluster to it (the first cluster)")
	fs.StringVar(&f.Domain, "domain", "", "control-plane domain: console at app.<domain>, API at api.<domain>, agents of added clusters at hub.<domain> (default <first --server address>.sslip.io; only with --control-plane)")
	fs.StringVar(&f.AdminEmail, "admin-email", "", "the control plane's administrator account (default admin@<domain>; only with --control-plane)")
	fs.StringArrayVar(&f.Servers, "server", nil, "control-plane node address (repeat three times for --ha ha)")
	fs.StringArrayVar(&f.Agents, "agent", nil, "agent node address (repeatable)")
	fs.StringVar(&f.HATier, "ha", "", "HA tier: single-server or ha (required, permanent)")
	fs.StringArrayVar(&f.Profiles, "profile", nil, "profile to install on top of core (repeatable)")
	fs.StringVar(&f.SSHUser, "ssh-user", "", "SSH user on the target nodes")
	fs.StringVar(&f.SSHKey, "ssh-key", "", "SSH private key file; defaults to ssh-agent or ~/.ssh/config")
	fs.StringVar(&f.StorageDevice, "storage-device", "", "blank device for the installer to create kubenest-vg on (omit if you created the volume group yourself)")
	fs.StringVar(&f.BackupTarget, "backup-target", "", "S3-compatible backup target for Velero (optional; unset reports backup: unconfigured)")
	return cmd
}

func newPlatformUninstallCommand() *cobra.Command {
	var (
		confirm     bool
		destroyData bool
		name        string
		f           InstallFlags
	)
	cmd := &cobra.Command{
		Use:   "uninstall --confirm",
		Short: "Remove k3s and every component the installer placed",
		Long: `Remove k3s and every component the installer placed, returning the machines
to a known state.

Uninstall never destroys data by default: persistent volumes survive. Pass
--destroy-data to remove them too — and even then the kubenest-vg volume
group is only removed if the installer created it. A volume group you created
yourself is never removed, on either path.

The node list and the volume-group ownership come from the install journal.
Without one, pass --server and --agent for the hosts to clean; no volume
group is removed in that case, because ownership that cannot be established
is treated as yours.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !confirm {
				return fmt.Errorf("uninstall removes the platform from every node of the cluster: pass --confirm to proceed")
			}
			return runUninstall(cmd.Context(), cmd.OutOrStdout(), name, destroyData, f)
		},
	}
	fs := cmd.Flags()
	fs.BoolVar(&confirm, "confirm", false, "confirm removal (required)")
	fs.BoolVar(&destroyData, "destroy-data", false, "also remove persistent volumes (never removes a volume group you created yourself)")
	fs.StringVar(&name, "name", "", "cluster name; defaults to the only install journal on this machine")
	fs.StringArrayVar(&f.Servers, "server", nil, "control-plane node address (only needed without an install journal)")
	fs.StringArrayVar(&f.Agents, "agent", nil, "agent node address (only needed without an install journal)")
	fs.StringVar(&f.SSHUser, "ssh-user", "", "SSH user on the target nodes")
	fs.StringVar(&f.SSHKey, "ssh-key", "", "SSH private key file; defaults to ssh-agent or ~/.ssh/config")
	return cmd
}

func newPlatformUpgradeCommand() *cobra.Command {
	var f UpgradeFlags

	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade the cluster to a newer platform bundle",
		Long: `Move a cluster from its current platform bundle to a newer one, as one
versioned unit: every component, and Kubernetes itself.

Components first, Kubernetes last. Everything before the kubernetes stage is a
Helm release and reverts in seconds; Kubernetes does not roll back at all, so
reverting it means restoring the datastore snapshot taken before anything
changed. Putting the irreversible step last means most failures cost seconds.

Every pre-flight gate runs before anything is touched, and the one that
matters most scans your live workloads for APIs the target Kubernetes version
removes. If it finds any, the upgrade is blocked and the report names them: an
upgrade that cleanly upgrades the cluster and takes your product down has
actively harmed you.`,
		Example: `  kubenest platform upgrade --cluster prod-1 --to 1.1

  # Accept one finding you have judged safe. There is no blanket override.
  kubenest platform upgrade --cluster prod-1 --to 1.1 \
    --acknowledge payments/Ingress/legacy-gateway`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.Cluster == "" {
				return fmt.Errorf("--cluster is required: which cluster to upgrade")
			}
			if f.To == "" {
				return fmt.Errorf("--to is required: the bundle version to upgrade to (see `kubenest platform diff`)")
			}
			return runUpgrade(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&f.Cluster, "cluster", "", "cluster to upgrade (required)")
	fs.StringVar(&f.To, "to", "", "bundle version to upgrade to (required)")
	fs.StringArrayVar(&f.Acknowledge, "acknowledge", nil, "accept one deprecated-API finding by namespace/Kind/name (repeatable; there is deliberately no blanket override)")
	fs.StringArrayVar(&f.Servers, "server", nil, "control-plane node address (only needed without a local install journal)")
	fs.StringArrayVar(&f.Agents, "agent", nil, "agent node address (only needed without a local install journal)")
	fs.StringVar(&f.SSHUser, "ssh-user", "", "SSH user on the target nodes")
	fs.StringVar(&f.SSHKey, "ssh-key", "", "SSH private key file; defaults to ssh-agent or ~/.ssh/config")
	return cmd
}

func newPlatformRollbackCommand() *cobra.Command {
	var (
		f       UpgradeFlags
		confirm bool
	)
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Return a cluster to the bundle it was upgraded from",
		Long: `Return a cluster to the bundle it was upgraded from.

What that means depends on where the upgrade stopped, and the two are not the
same thing:

  Before the kubernetes stage — every component is a Helm release and reverts
  to its previous revision in seconds. Nothing is lost.

  After it — Kubernetes does not downgrade, so this restores the datastore
  snapshot taken before the upgrade began. That is a genuine restore with a
  service interruption, and it does NOT touch your persistent volumes:
  database contents and uploaded files survive, because rolling application
  data back to the start of the window would discard every transaction since.

The mechanism is reported before anything happens, and the expensive one asks
for confirmation.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.Cluster == "" {
				return fmt.Errorf("--cluster is required: which cluster to roll back")
			}
			return runRollback(cmd.Context(), cmd.OutOrStdout(), cmd.InOrStdin(), f, confirm)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&f.Cluster, "cluster", "", "cluster to roll back (required)")
	fs.StringVar(&f.To, "to", "", "the bundle version the upgrade was moving to; defaults to the journal's")
	fs.BoolVar(&confirm, "confirm", false, "confirm a datastore restore, which interrupts service")
	fs.StringArrayVar(&f.Servers, "server", nil, "control-plane node address (only needed without a local install journal)")
	fs.StringArrayVar(&f.Agents, "agent", nil, "agent node address (only needed without a local install journal)")
	fs.StringVar(&f.SSHUser, "ssh-user", "", "SSH user on the target nodes")
	fs.StringVar(&f.SSHKey, "ssh-key", "", "SSH private key file; defaults to ssh-agent or ~/.ssh/config")
	return cmd
}
