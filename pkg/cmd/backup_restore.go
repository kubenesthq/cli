package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/upgrade"
)

// `kubenest backup restore` — the everyday restore path (PLAN 7.5, kn-x0wv and
// kn-t43).
//
// TWO MODES, ONE FLAG SET. `--namespace N` restores a whole namespace;
// `--namespace N --pvc P [--pvc P...]` refills a workload's stranded volumes in
// place. A restore leaves three follow-on commands behind, and they are this
// same verb: `--resume <id>` continues an interrupted restore and never
// activates anything, `--activate <id>` is the explicit decision to let the
// namespace run again, and `--abort <id>` gives up while leaving the project
// paused.
//
// ALL THE WORK IS IN pkg/backup. This file owns the flag surface and its
// refusals, the transport, and where the operation record lives — and nothing
// else, so that "what the command does" is tested with no cluster (pkg/backup's
// tests) and "what the command accepts" is tested with no transport
// (backup_restore_test.go).
type backupRestore struct {
	out   io.Writer
	in    io.Reader
	f     backup.RestoreOptions
	state restoreFlagState
	// open builds the run's cluster, eligibility source and operation record.
	// It is a field because a test drives the command past the transport with a
	// fake eligibility source, which is how the plan's rendering and its
	// refusals are asserted at the command's own surface.
	open func(ctx context.Context) (backup.RestoreDeps, io.Closer, error)
}

// restoreFlagState is the raw flag state, before it becomes the run's options.
type restoreFlagState struct {
	from          string
	latest        bool
	namespace     string
	replace       bool
	pvcs          []string
	includeJobs   bool
	resume        string
	activate      string
	abort         string
	acceptDataAge bool
	confirm       bool
	keepRestored  bool
	keepDesired   bool
}

// validate refuses the flag combinations before anything is read.
//
// THE REFUSALS ARE THE CONTRACT. Several flags mean a different thing in each
// mode, and one silently accepted for a mode it does not apply to would leave
// an operator believing the run was scoped to something it never looked at.
func (s restoreFlagState) validate() error {
	// The activation question, asked first so its refusals are the ones an
	// operator sees: these two flags are the ONLY pair that belongs to a
	// follow-on command rather than to a new restore.
	if s.keepRestored && s.keepDesired {
		return fmt.Errorf("--keep-restored and --keep-desired are mutually exclusive: one of the two states wins")
	}
	if (s.keepRestored || s.keepDesired) && s.activate == "" {
		return fmt.Errorf("--keep-restored and --keep-desired answer activation's question, so they belong to `--activate <operation-id>`")
	}
	chosen := []string{}
	if s.activate != "" {
		chosen = append(chosen, "--activate")
	}
	if s.abort != "" {
		chosen = append(chosen, "--abort")
	}
	if s.resume != "" {
		chosen = append(chosen, "--resume")
	}
	if len(chosen) > 1 {
		return fmt.Errorf("%s are mutually exclusive: a restore is resumed, activated or aborted, never two of them at once", joinWords(chosen))
	}
	if len(chosen) == 1 {
		// A follow-on command acts on the record, so it takes none of the flags
		// that describe a new restore. --namespace stays allowed (and optional):
		// it names the namespace the record is about.
		other := []struct {
			name string
			set  bool
		}{
			{"--from", s.from != ""},
			{"--latest", s.latest},
			{"--replace", s.replace},
			{"--pvc", len(s.pvcs) > 0},
			{"--include-jobs", s.includeJobs},
			{"--accept-data-age", s.acceptDataAge},
		}
		for _, o := range other {
			if o.set {
				return fmt.Errorf("%s takes no other mode flags: it acts on an existing operation's record, and %s describes a new restore", chosen[0], o.name)
			}
		}
		return nil
	}
	if s.namespace == "" {
		return fmt.Errorf("--namespace is required: it names the namespace a restore is about")
	}
	switch {
	case s.from != "" && s.latest:
		return fmt.Errorf("--from and --latest are mutually exclusive: name the backup, or let the command pick the newest eligible one")
	case s.from == "" && !s.latest:
		return fmt.Errorf("choose a backup: pass --from NAME or --latest")
	}
	if len(s.pvcs) > 0 {
		// Mode 2 refills named claims in a live namespace and restores nothing
		// else, so the two whole-namespace flags mean nothing there.
		switch {
		case s.replace:
			return fmt.Errorf("--replace is mode 1's (--namespace alone): a volume restore refills the claims you name and never deletes the namespace, so there is nothing to replace")
		case s.includeJobs:
			return fmt.Errorf("--include-jobs is mode 1's (--namespace alone): a volume restore restores no workloads, so it restores no Jobs either")
		}
	}
	return nil
}

// options is the flag state as the run's options. Validation has already run;
// the namespace may still be empty for a follow-on command, and the record
// fills it in.
func (s restoreFlagState) options() backup.RestoreOptions {
	return backup.RestoreOptions{
		Namespace:     s.namespace,
		From:          s.from,
		Latest:        s.latest,
		Replace:       s.replace,
		PVCs:          s.pvcs,
		IncludeJobs:   s.includeJobs,
		Resume:        s.resume,
		Activate:      s.activate,
		Abort:         s.abort,
		AcceptDataAge: s.acceptDataAge,
		Confirm:       s.confirm,
		KeepRestored:  s.keepRestored,
		KeepDesired:   s.keepDesired,
	}
}

func joinWords(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	}
	out := ""
	for i, w := range words[:len(words)-1] {
		if i > 0 {
			out += ", "
		}
		out += w
	}
	return out + " and " + words[len(words)-1]
}

// newBackupRestoreCommand is the command itself.
func newBackupRestoreCommand() *cobra.Command {
	var (
		conn  backupConn
		state restoreFlagState
	)
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore a namespace, or one workload's volumes, from a backup",
		Long: `Restore from a Velero backup into the live cluster.

MODE 1 — a whole namespace: --namespace N with --from NAME or --latest. The
command prints a restore plan (the backup, its completion time and conservative
data age, its coverage and consistency method, the namespace and claim
identities, the workloads it will stop, what will be discarded, and the
recovery-point verdict), waits for confirmation, pauses the project's
reconcilers, waits for the operator to acknowledge the pause, takes a safety
backup, stops the writers, deletes the namespace and restores it. Jobs are
excluded unless --include-jobs, and CronJobs come back suspended.

MODE 2 — one workload's stranded volumes, after a node loss:
--namespace N --pvc P [--pvc P...]. The claims are deleted and refilled on live
nodes, the workloads holding them are held at zero until every named claim's
PodVolumeRestore is Completed, and every volume of the same workload that is NOT
named keeps its current contents, byte for byte.

EITHER MODE ENDS AT "restored — awaiting activation": the data is back and the
project stays paused. Then:

  kubenest backup restore --activate <operation-id>   let the namespace run again
  kubenest backup restore --resume <operation-id>     continue an interrupted restore
  kubenest backup restore --abort <operation-id>      give up; the project stays paused

The hold is the annotation kubenest.io/reconcile-paused on the project's Project
resource in kubenest-system, so it survives the namespace's deletion. The
operation record in kube-system is the lock: a second operator is refused while
this one runs, and an interrupted run is resumable from any laptop.`,
		Example: `  kubenest backup restore --cluster prod-1 --namespace payments --latest --replace \
    --server 10.0.1.10 --ssh-user ubuntu --bundle-manifest bundles/platform-1.1.yaml
  kubenest backup restore --cluster prod-1 --namespace payments --pvc data-0 \
    --from daily-20260813-0200 --server 10.0.1.10 --bundle-manifest bundles/platform-1.1.yaml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			b := &backupRestore{
				out:   cmd.OutOrStdout(),
				in:    cmd.InOrStdin(),
				state: state,
				f:     state.options(),
			}
			b.open = func(ctx context.Context) (backup.RestoreDeps, io.Closer, error) {
				return b.dial(ctx, conn)
			}
			return b.run(cmd.Context())
		},
	}
	conn.register(cmd)
	fs := cmd.Flags()
	fs.StringVar(&state.from, "from", "", "name of the backup to restore")
	fs.BoolVar(&state.latest, "latest", false, "restore the newest ELIGIBLE backup, saying which newer ones it passed over and why")
	fs.StringVar(&state.namespace, "namespace", "", "namespace to restore (required for a new restore; the record names it for --resume/--activate/--abort)")
	fs.BoolVar(&state.replace, "replace", false, "restore over the namespace even though it exists (an absent namespace needs no --replace)")
	fs.StringArrayVar(&state.pvcs, "pvc", nil, "refill this claim in place (repeatable); selects the volume mode for after a node loss")
	fs.BoolVar(&state.includeJobs, "include-jobs", false, "restore Jobs deliberately; they run at activation")
	fs.StringVar(&state.resume, "resume", "", "continue an interrupted restore by operation id; never activates anything")
	fs.StringVar(&state.activate, "activate", "", "let a restored namespace run again, by operation id")
	fs.StringVar(&state.abort, "abort", "", "give up on a restore by operation id; the project stays paused")
	fs.BoolVar(&state.acceptDataAge, "accept-data-age", false, "accept a backup older than the bundle's recovery-point policy")
	fs.BoolVar(&state.confirm, "confirm", false, "confirm the printed plan without a prompt (the non-interactive path)")
	fs.BoolVar(&state.keepRestored, "keep-restored", false, "activation: the restored configuration wins over the current desired state")
	fs.BoolVar(&state.keepDesired, "keep-desired", false, "activation: the current desired state wins over the restored configuration")
	return cmd
}

// run validates and then runs, so every refusal happens before a connection is
// opened.
func (b *backupRestore) run(ctx context.Context) error {
	if err := b.state.validate(); err != nil {
		return err
	}
	deps, closer, err := b.open(ctx)
	if err != nil {
		return err
	}
	if closer != nil {
		defer closer.Close()
	}
	return backup.RunRestore(ctx, b.out, b.in, b.f, deps)
}

// dial builds the run's dependencies over the SSH transport every other backup
// command uses: the cluster, the eligibility source, and the operation record.
//
// THE RECORD LIVES IN THE CLUSTER and is what makes the run resumable from any
// laptop. The control-plane mirror is attached when this machine is logged in
// and the install journal names the cluster id; neither is required, because a
// restore must work on the cluster whose control plane is the thing that may
// just have died.
func (b *backupRestore) dial(ctx context.Context, conn backupConn) (backup.RestoreDeps, io.Closer, error) {
	if err := conn.validate(); err != nil {
		return backup.RestoreDeps{}, nil, err
	}
	bundle, err := manifest.Load(conn.BundlePath)
	if err != nil {
		return backup.RestoreDeps{}, nil, err
	}
	ep, err := sshx.Resolve(conn.Servers[0], sshx.Options{User: conn.SSHUser, KeyPath: conn.SSHKey})
	if err != nil {
		return backup.RestoreDeps{}, nil, err
	}
	client, err := sshx.Dial(ctx, ep, sshx.Options{KeyPath: conn.SSHKey})
	if err != nil {
		return backup.RestoreDeps{}, nil, err
	}
	b.f.Bundle = bundle
	b.f.Cluster = conn.Cluster
	store := &operation.Store{Runner: client, Operator: upgrade.OperatorName()}
	store.Mirror, store.MirrorClusterID = restoreMirror(conn.Cluster)
	deps := backup.RestoreDeps{
		Cluster: backup.NewK3sCluster(client),
		Backups: backup.NewVeleroBackups(client),
		Store:   store,
		Runner:  client,
	}
	return deps, client, nil
}

// restoreMirror returns the control-plane client and cluster id to mirror the
// record to, or nils when this machine cannot provide them. A mirror that
// cannot be built is not a failed restore: the record in the cluster is the
// lock, and the console's view of it is a nicety beside that.
func restoreMirror(clusterName string) (*api.Client, string) {
	client, err := controlPlaneClient()
	if err != nil {
		return nil, ""
	}
	path, err := install.JournalPath(clusterName)
	if err != nil {
		return nil, ""
	}
	journal, err := install.ReadJournal(path)
	if err != nil || journal == nil || journal.ClusterID == "" {
		return nil, ""
	}
	return client, journal.ClusterID
}
