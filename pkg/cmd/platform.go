package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// The platform command surface follows docs.kubenest.io/platform/install. The
// flag surface and its validation land here so the installer (kn-7k8) plugs
// into a settled interface; until it lands, the subcommands say so plainly
// instead of pretending.

// AnnotationUnavailable marks a registered command that is not built yet.
//
// It is the machine-readable half of the skeleton rule. The metadata generator
// (cmd/gen-command-metadata) reads it and reports the path as available: false,
// so the docs checker
// (kubenest-docs/scripts/check_examples_against_cli.py) refuses a runnable
// example of a stub by asking the command tree rather than by matching a
// sentence in the help text. The runtime half is a RunE that refuses non-zero;
// both are set together, or a path reports available and exits non-zero.
//
// No command carries it today (the wave-1 surface is implemented), and it stays
// because the generator reads it: the next stub is marked with it rather than
// by inventing a second convention.
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
	// FleetRecipient and InstanceID are the fleet's identity. A
	// --control-plane install generates the key and records both; a cluster
	// added to a fleet normally reads them from this machine's config, which
	// that install wrote, and only needs the flags when it is installed from a
	// machine that never ran it.
	FleetRecipient string
	InstanceID     string

	// Cluster is --cluster: the cluster's IMMUTABLE id, which is what a
	// recovery adopts by. It is deliberately a different flag from --name: a
	// display name never authorises adoption, and two clusters can share one.
	Cluster string
	// RestoreFrom is --restore-from: what a recovery starts from. "latest" is
	// the newest eligible recovery set for this cluster.
	RestoreFrom string
	// RecoveryKit is --recovery-kit: where the kit comes from. "s3" is the
	// bucket the recovery sets and kits live in.
	RecoveryKit string
	// FleetKeyFile is the file holding the fleet recovery key. It is a file
	// rather than a flag value for the reason every other credential is: a
	// secret on a command line lands in shell history and in `ps`.
	FleetKeyFile string
	// Backup names one backup instead of the recovery set's newest.
	Backup string
	// OldHostFenced is the operator's explicit confirmation that the machine
	// being replaced cannot come back. A recovery refuses without it.
	OldHostFenced bool
	// AcknowledgeSingleOperator is the F20 acknowledgement, needed only when
	// the backup target cannot create an object conditionally.
	AcknowledgeSingleOperator bool
	// AdminPassword is the control-plane administrator's password, needed only
	// by a --control-plane recovery: the accounts are in the restored
	// database, so the recovery signs in as the operator who already exists.
	AdminPassword string
}

// recovering reports whether this is a recovery install.
func (f InstallFlags) recovering() bool { return f.RestoreFrom != "" || f.RecoveryKit != "" }

// recoveringCluster reports whether this is a WORKLOAD cluster recovery, which
// is the shape selected by an immutable id rather than by a name.
func (f InstallFlags) recoveringCluster() bool { return f.recovering() && !f.ControlPlane }

// validateRecovery owns the refusals a recovery install makes before anything
// is read from anywhere.
//
// THE TWO FLAGS ARE A PAIR. `--restore-from` says what to rebuild from and
// `--recovery-kit` says where the kit that opens it comes from; either alone is
// a command that cannot do what it says, and accepting one would leave the
// operator believing a recovery was configured when none was.
func (f *InstallFlags) validateRecovery() error {
	switch {
	case f.RestoreFrom == "":
		return fmt.Errorf("--recovery-kit needs --restore-from: name what the recovery starts from (today: latest)")
	case f.RecoveryKit == "":
		return fmt.Errorf("--restore-from needs --recovery-kit: name where the recovery kit comes from (today: s3), because a recovery with no kit has no repository password and no join token")
	case f.RestoreFrom != "latest":
		return fmt.Errorf("--restore-from %q is not something this release recovers from: the only value is latest, the newest eligible recovery set for the cluster", f.RestoreFrom)
	case f.RecoveryKit != "s3":
		return fmt.Errorf("--recovery-kit %q is not a kit source this release has: the only value is s3", f.RecoveryKit)
	}
	if f.BackupTarget == "" {
		return fmt.Errorf("a recovery needs --backup-target: it is the only thing that says which bucket holds this cluster's recovery set and backup, and the fleet recovery key alone does not name a store (S3 credentials come from KUBENEST_BACKUP_ACCESS_KEY_ID / KUBENEST_BACKUP_SECRET_ACCESS_KEY)")
	}
	if f.FleetKeyFile == "" {
		return fmt.Errorf("a recovery needs --fleet-key-file: the fleet recovery key is what opens the kit, and it is read from a file rather than taken on the command line so it does not land in shell history or in `ps`")
	}
	// The fencing confirmation is flag shape, and it is checked here as well as
	// in the recovery pre-flight: this is the earliest point an operator can be
	// told, and the stage check is what protects a caller that did not come
	// through this command.
	if !f.OldHostFenced {
		return fmt.Errorf("a recovery needs --old-host-fenced: power the machine being replaced off at its provider, or make it unreachable, then confirm it. An old host that rejoins after the new one registers is two clusters behind one identity, and every operation after that is against whichever answered")
	}
	if f.ControlPlane {
		// A control-plane recovery re-registers the MANAGEMENT cluster, so it
		// is named — by the name its own restored record already carries.
		if f.Cluster != "" {
			return fmt.Errorf("--cluster does not apply to a --control-plane recovery: the id of the management cluster this rebuilds is in the recovery set's binding, and naming it here could only disagree with the set")
		}
		return nil
	}
	// A WORKLOAD CLUSTER IS ADOPTED BY ITS IMMUTABLE ID, NEVER BY ITS NAME.
	if f.Cluster == "" {
		return fmt.Errorf("--restore-from needs --cluster <cluster-id>: a recovery adopts a cluster by its IMMUTABLE id, and a display name never authorises adoption — two clusters can carry the same name, and the one that comes back would be whichever the control plane answered with first. The id is in the recovery set's binding, or in `kubenest cluster list`")
	}
	if f.Name != "" {
		return fmt.Errorf("--name does not select anything in a recovery and is not accepted here: the cluster this rebuilds is chosen by --cluster <cluster-id>, which no display name can stand in for. Remove --name; the name comes back from the cluster's own record")
	}
	return nil
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
	if f.recovering() {
		if err := f.validateRecovery(); err != nil {
			return err
		}
	}
	if f.Name == "" && !f.recoveringCluster() {
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
	fs.StringVar(&f.FleetRecipient, "fleet-recipient", "", "the fleet recovery key's PUBLIC recipient (age1...); normally read from this machine's config after a --control-plane install")
	fs.StringVar(&f.InstanceID, "instance-id", "", "the instance id every recovery kit is bound to; normally read from this machine's config after a --control-plane install")
	fs.StringVar(&f.Cluster, "cluster", "", "the cluster's IMMUTABLE id, for a recovery: --restore-from adopts the cluster this names, and a display name never authorises adoption")
	fs.StringVar(&f.RestoreFrom, "restore-from", "", "RECOVER an existing cluster instead of installing a new one: the newest eligible recovery set (today: latest)")
	fs.StringVar(&f.RecoveryKit, "recovery-kit", "", "where the recovery kit comes from, with --restore-from (today: s3, the bucket --backup-target names)")
	fs.StringVar(&f.FleetKeyFile, "fleet-key-file", "", "file holding the fleet recovery key (the AGE-SECRET-KEY-1... printed once at control-plane install); required by a recovery")
	fs.StringVar(&f.Backup, "backup", "", "name one backup the recovery set must record as completed, instead of its newest")
	fs.BoolVar(&f.OldHostFenced, "old-host-fenced", false, "confirm the machine being replaced is powered off or unreachable; a recovery refuses without it")
	fs.BoolVar(&f.AcknowledgeSingleOperator, "acknowledge-single-operator", false, "acknowledge the documented single-operator rule when the backup target cannot create an object conditionally (F20)")
	fs.StringVar(&f.AdminPassword, "admin-password", "", "the control-plane administrator's password, for a --control-plane recovery: the accounts are in the restored database (or KUBENEST_ADMIN_PASSWORD)")
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
			if f.Cluster == "" && !f.ControlPlane {
				return fmt.Errorf("--cluster is required: which cluster to upgrade")
			}
			if f.To == "" {
				return fmt.Errorf("--to is required: the bundle version to upgrade to (see `kubenest platform diff`)")
			}
			if f.Resume != "" && !f.ControlPlane {
				return fmt.Errorf("--resume continues an interrupted CONTROL-PLANE upgrade, so it needs --control-plane: a workload cluster's upgrade resumes by re-running the identical command, which reads its own journal")
			}
			if f.Now && f.Wait {
				return fmt.Errorf("--now and --wait ask for opposite things: --now acts immediately regardless of the maintenance window, --wait holds until the window opens")
			}
			return runUpgrade(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&f.Cluster, "cluster", "", "cluster to upgrade (required)")
	fs.StringVar(&f.To, "to", "", "bundle version to upgrade to (required)")
	fs.BoolVar(&f.Wait, "wait", false, "hold until the maintenance window opens, holding nothing while waiting, then take the operation lock and re-run every gate")
	fs.BoolVar(&f.Now, "now", false, "act immediately regardless of the maintenance window; --now bypasses the window and nothing else")
	fs.StringArrayVar(&f.Acknowledge, "acknowledge", nil, "accept one deprecated-API finding by namespace/Kind/name (repeatable; there is deliberately no blanket override)")
	fs.StringArrayVar(&f.Servers, "server", nil, "control-plane node address (only needed without a local install journal)")
	fs.StringArrayVar(&f.Agents, "agent", nil, "agent node address (only needed without a local install journal)")
	fs.StringVar(&f.SSHUser, "ssh-user", "", "SSH user on the target nodes")
	fs.StringVar(&f.SSHKey, "ssh-key", "", "SSH private key file; defaults to ssh-agent or ~/.ssh/config")
	addControlPlaneUpgradeFlags(cmd, &f)
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
