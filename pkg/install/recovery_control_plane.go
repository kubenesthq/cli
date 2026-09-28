package install

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/controlplane"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/recovery"
	"kubenest.io/cli/pkg/recoverykit"
)

// The control-plane recovery's own stages (bead T4.8, PLAN 7.8's "Restore it").
const (
	// StageRecoveryControlPlane installs the control-plane chart with the
	// kit's authority and key material, and with the backend HELD.
	StageRecoveryControlPlane = "recovery-control-plane"
	// StageRecoveryCheckpoint loads the newest eligible checkpoint into the
	// held database, migrates only if the chart is newer, and starts the
	// backend.
	StageRecoveryCheckpoint = "recovery-checkpoint"
	// StageRecoveryProvisional compares the restored desired state with what
	// surviving clusters report, and shows the differences for a decision.
	StageRecoveryProvisional = "recovery-provisional"
)

// controlPlaneRecoveryPlan is the ordered procedure for rebuilding an all-in-one
// host: a fresh management cluster, the control plane restored from its newest
// eligible checkpoint, and then the management cluster's own namespaces.
//
// THE OLD MANAGEMENT CLUSTER'S DATASTORE IS NEVER REPLAYED on this path (PLAN
// 7.8): its Kubernetes objects describe a cluster whose host is gone, and
// restoring them would put the old control plane's objects over the new one.
// What comes back is the DATABASE — organisations, members, roles, windows,
// alert routes, inventories and token floors — through a checkpoint, which is a
// logical artifact rather than a copy of a host.
//
// No stage here writes a recovery kit. The kit is what this recovery was
// selected from; a new one would be a new artifact, and the set that names this
// instance would stop describing anything.
func controlPlaneRecoveryPlan(s *Session, bind stageBinder) []Stage {
	return []Stage{
		{Name: StageRecoverySelect, AlwaysRun: true, Run: bind(stageRecoverySelectControlPlane)},
		{Name: StagePreflight, AlwaysRun: true, Run: bind(stagePreflight)},
		{Name: StageRecoveryPreflight, Run: bind(stageRecoveryPreflightControlPlane)},
		{Name: StageRecoveryOwnership, Run: bind(stageRecoveryOwnership)},
		{Name: StageK3sServer, Component: "k3s", Run: bind(stageK3sServer)},
		{Name: StageK3sAgents, Component: "k3s", Run: bind(stageK3sAgents)},
		{Name: StageNetworking, Component: "traefik", Run: bind(stageNetworking)},
		{Name: StageCerts, Component: "cert-manager", Run: bind(stageCerts)},
		{Name: StageStorage, Component: "openebs-lvm-localpv", Run: bind(stageStorage)},
		// The chart goes on with the kit's CA and key material, and the
		// backend HELD: started on an empty database it would build the schema
		// from its own models, and the restore that followed would fail at its
		// first table (probe P3 question 3, arm B).
		{Name: StageRecoveryControlPlane, AlwaysRun: true, Run: bind(stageRecoveryControlPlane)},
		{Name: StageRecoveryCheckpoint, Run: bind(stageRecoveryCheckpoint)},
		// The management cluster's repository password, before Velero starts:
		// its own workloads come back from that repository, and Velero writes
		// its vendored default into an absent Secret (probe P3 question 2).
		{Name: StageRecoveryRepository, Run: bind(stageRecoveryRepository)},
		// The management cluster registers through the control plane it now
		// hosts, by the normal path.
		{Name: StageRegister, AlwaysRun: true, Run: bind(stageRegister)},
		{Name: StageBackup, Component: "velero", Run: bind(stageBackup)},
		{Name: StageDay2, Component: "system-upgrade-controller", Run: bind(stageDay2)},
		{Name: StageBackupTarget, Component: "velero", Run: bind(stageBackupTarget)},
		{Name: StageAgent, Component: "kubenest-agent", Run: bind(stageAgent)},
		// Step 4: what surviving clusters report against what the restored
		// database holds, shown rather than pushed.
		{Name: StageRecoveryProvisional, Run: bind(stageRecoveryProvisional)},
		// Step 5: the management cluster's own workload namespaces.
		{Name: StageRecoveryRestore, Run: bind(stageRecoveryRestore)},
		{Name: StageProfiles, Run: bind(stageProfiles)},
		{Name: StageRecord, Run: bind(stageRecord)},
		{Name: StageVerify, AlwaysRun: true, Run: bind(Verify)},
		{Name: StageRecoveryActivate, Run: bind(stageRecoveryActivate)},
	}
}

// stageRecoverySelectControlPlane finds the instance's recovery set.
//
// IT CANNOT SELECT BY CLUSTER ID: the operator recovering an all-in-one host is
// reading the bucket precisely because the control plane that would tell them
// the management cluster's id is the thing that is gone. The bucket prefix does
// that job instead (recovery.SelectControlPlane), and the set's binding — which
// names the instance and the management cluster — is checked against the
// instance this recovery was told about before anything is opened.
func stageRecoverySelectControlPlane(ctx context.Context, s *Session) error {
	rec := s.Opts.Recovery
	if rec == nil {
		return errors.New("the recovery-selection stage ran on an install that is not a recovery")
	}
	target, err := s.recoveryTarget()
	if err != nil {
		return err
	}
	client, err := target.S3Client()
	if err != nil {
		return err
	}
	key, err := recoverykit.ParseFleetKey(rec.FleetKey)
	if err != nil {
		return fmt.Errorf("reading the fleet recovery key this recovery holds: %w", err)
	}
	s.recoveryRecipient = key.Recipient()

	sel, err := recovery.SelectControlPlane(ctx, client, recovery.Scope(recoverykit.Location{Prefix: target.Prefix}), s.instanceIdentity(), "")
	if err != nil {
		return err
	}
	if sel.Set.Binding.ClusterID == "" {
		return fmt.Errorf("the control-plane recovery set at %s is bound to no management cluster, so a restored control plane would have no cluster of its own to report into", sel.SetKey)
	}
	// AND THE MANAGEMENT CLUSTER'S OWN SET, which is where its workload backups
	// live: the control-plane set binds the instance's artifacts, and the
	// cluster the control plane runs in has its own set beside it (PLAN 7.8).
	// Without this the database would come back and the management workload
	// would not, which is the half of an all-in-one host that holds data.
	// The CONTROL PLANE's own principal, for every read under control-plane/.
	// Its absence is refused here, before anything is read, because the
	// alternative is an AccessDenied from the store that says nothing about
	// which of the two credentials is missing.
	checkpointStore, err := s.checkpointStore()
	if err != nil {
		return err
	}
	workload, err := recovery.SelectManagementCluster(ctx, client,
		recovery.Scope(recoverykit.Location{Prefix: target.Prefix}),
		sel.Set.Binding.InstanceID, sel.Set.Binding.ClusterID, "")
	if err != nil {
		return err
	}
	s.recoveryStore = client
	s.recoveryCheckpointStore = checkpointStore
	s.recoveryTargetValue = target
	s.recoverySel = sel
	s.recoveryWorkloadSel = workload
	s.kit = sel.Kit
	s.Jnl.ClusterID = sel.Set.Binding.ClusterID
	s.Logf("  recovery: instance kit artifact %s from %s, for the management cluster %s", sel.Set.ArtifactID, sel.SetKey, sel.Set.Binding.ClusterID)
	s.Logf("  recovery: the management cluster's own workloads come from %s, backup %q", workload.SetKey, workload.Backup.Name)
	return s.saveRecord()
}

// stageRecoveryPreflightControlPlane asks the four questions, refuses an
// unfenced old host or one too small for the management cluster's own volumes,
// and refuses a checkpoint this build cannot serve.
func stageRecoveryPreflightControlPlane(ctx context.Context, s *Session) error {
	rec := s.Opts.Recovery
	if rec == nil {
		return errors.New("the recovery pre-flight ran on an install that is not a recovery")
	}
	if s.recoverySel == nil || s.recoveryStore == nil {
		return errors.New("no recovery set was selected: the select stage runs first")
	}
	answers, err := recovery.Verify(ctx, s.recoveryStore, s.recoverySel, rec.FleetKey)
	if err != nil {
		return err
	}
	for _, answer := range []struct {
		what string
		a    recovery.Answer
	}{
		{"kit upload intact", answers.Upload},
		{"opens with the fleet key supplied", answers.Decryption},
		{"fingerprints match", answers.Fingerprints},
		{"recovery set complete and about this instance", answers.Set},
	} {
		mark := "no "
		if answer.a.OK {
			mark = "yes"
		}
		s.Logf("  recovery check [%s] %s: %s", mark, answer.what, answer.a.Detail)
	}
	if !answers.AllGood() {
		return fmt.Errorf("the instance's recovery set is not usable: %s. Nothing has been changed anywhere", strings.Join(answers.Failed(), "; "))
	}
	if !rec.OldHostFenced {
		return errors.New("recovery replaces a machine, so it needs the operator to confirm the old one is FENCED first: power it off at the provider, or make it unreachable, then pass --old-host-fenced. Nothing has been changed")
	}
	for _, node := range s.NodesWithRole(RoleServer) {
		capacity, err := recovery.CheckCapacity(ctx, node.Runner, s.recoverySel.Set, s.Opts.StorageDevice)
		s.Logf("  recovery check [%s] capacity on %s: %s", yesNo(err == nil || capacity.Known), node.Address, capacity.Report())
		if err != nil {
			return err
		}
	}

	// The checkpoint this recovery would load, and whether this build can
	// serve it. Refused here, before the chart goes on: a checkpoint from a
	// newer control plane carries a schema this code does not know, and a
	// different Postgres major is a migration rather than a recovery.
	cp, sealed, err := s.selectCheckpoint(ctx)
	if err != nil {
		return err
	}
	chart, err := controlplane.ChartVersionOf()
	if err != nil {
		return err
	}
	major, image, err := controlplane.ChartPinnedPostgres()
	if err != nil {
		return err
	}
	if err := recovery.CheckCheckpoint(checkpointView{cp}, recovery.ControlPlaneTarget{
		Version:       chart.Version,
		PostgresMajor: major,
		PostgresImage: image,
	}); err != nil {
		return err
	}
	needed, err := controlplane.MigrationNeeded(controlplane.ControlPlaneVersion{Version: cp.ControlPlaneVersion}, chart.Version)
	if err != nil {
		return err
	}
	s.recoveryCheckpoint = cp
	s.recoveryCheckpointDump = sealed
	s.recoveryMigrationNeeded = needed
	s.Logf("  recovery checkpoint: %s, taken %s by control plane %s on PostgreSQL %d (%s)",
		cp.Key, cp.At, cp.ControlPlaneVersion, cp.Dump.PostgresMajor, cp.Dump.PostgresImage)
	s.Logf("  recovery checkpoint: it includes security changes up to %s; the migration Job will %s run",
		orNone(cp.IncludesSecurityChangeAt), map[bool]string{true: "", false: "NOT "}[needed])
	return nil
}

// checkpointView adapts the bucket's checkpoint manifest to the eligibility
// rules pkg/recovery applies.
type checkpointView struct {
	cp *controlplane.BucketCheckpoint
}

func (v checkpointView) CheckpointVersion() string       { return v.cp.ControlPlaneVersion }
func (v checkpointView) CheckpointPostgresMajor() int    { return v.cp.Dump.PostgresMajor }
func (v checkpointView) CheckpointPostgresImage() string { return v.cp.Dump.PostgresImage }
func (v checkpointView) CheckpointUploadComplete() bool {
	return v.cp.Key != "" && v.cp.Envelope.SHA256 != ""
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none recorded"
	}
	return s
}

// selectCheckpoint lists the instance's checkpoints in the bucket and picks the
// newest eligible one, with its sealed dump.
func (s *Session) selectCheckpoint(ctx context.Context) (*controlplane.BucketCheckpoint, []byte, error) {
	if s.recoveryCheckpointStore == nil {
		return nil, nil, errors.New("this recovery has no checkpoint principal, so it cannot read the control plane's checkpoints: set KUBENEST_CONTROL_PLANE_CHECKPOINT_ACCESS_KEY_ID and KUBENEST_CONTROL_PLANE_CHECKPOINT_SECRET_ACCESS_KEY (KUBENEST_CHECKPOINT_* is accepted too). The checkpoints live under the bucket's control-plane/ prefix, which PLAN 7.8 gives its own credential, separate from every cluster's")
	}
	return controlplane.SelectControlPlaneCheckpoint(ctx, s.recoveryCheckpointStore.List, s.recoveryCheckpointStore.Get, s.checkpointPrefix())
}

// checkpointCredentials are the control plane's own object-store principal.
// They are read from the control plane's variables first and never from the
// cluster's pair: a recovery that used the cluster's credential here would be
// sharing one principal between a cluster's backups and the control plane's
// database, which is the boundary `backup set-target` exists to prove.
func checkpointCredentials() (keyID, secret string) {
	keyID = envFirst("KUBENEST_CONTROL_PLANE_CHECKPOINT_ACCESS_KEY_ID", "KUBENEST_CHECKPOINT_ACCESS_KEY_ID", "AWS_ACCESS_KEY_ID")
	secret = envFirst("KUBENEST_CONTROL_PLANE_CHECKPOINT_SECRET_ACCESS_KEY", "KUBENEST_CHECKPOINT_SECRET_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY")
	return keyID, secret
}

// checkpointStore is the bucket reached with that principal. It is the store
// every read under control-plane/ goes through: the checkpoint listing, each
// manifest, and the sealed dump.
func (s *Session) checkpointStore() (recovery.Store, error) {
	keyID, secret := checkpointCredentials()
	if keyID == "" || secret == "" {
		return nil, errors.New("a control-plane recovery needs the CONTROL PLANE's object-store credentials to read its checkpoints: set KUBENEST_CONTROL_PLANE_CHECKPOINT_ACCESS_KEY_ID and KUBENEST_CONTROL_PLANE_CHECKPOINT_SECRET_ACCESS_KEY (KUBENEST_CHECKPOINT_* is accepted too). They are not the cluster's KUBENEST_BACKUP_* pair: PLAN 7.8 gives the checkpoints their own principal, which reaches the bucket's control-plane/ prefix and nothing else, and the cluster's principal reaches only that cluster's prefix — so neither can do the other's reading")
	}
	target, err := s.recoveryTarget()
	if err != nil {
		return nil, err
	}
	// The same store, the other principal: endpoint, bucket and region come
	// from --backup-target, and the prefix is the control plane's own.
	checkpointTarget := backup.Target{
		Endpoint:        target.Endpoint,
		Bucket:          target.Bucket,
		Region:          target.Region,
		Prefix:          backup.ControlPlanePrefix,
		AccessKeyID:     keyID,
		SecretAccessKey: secret,
	}
	client, err := checkpointTarget.S3Client()
	if err != nil {
		return nil, fmt.Errorf("building the control plane's own object-store client: %w", err)
	}
	return client, nil
}

// checkpointPrefix is where the control plane's checkpoints live, and the
// answer is the WRITER's: `backup.ControlPlanePrefix` at the BUCKET ROOT.
//
// `--backup-target`'s prefix scopes a cluster's own artifacts (its kits, sets,
// backups and datastore snapshots), and `controlplane.NewCheckpointTarget`
// deliberately does NOT put it in front of a checkpoint: the control plane's
// checkpoints are the instance's, they go under `<bucket>/control-plane/*`, and
// that is the only path the checkpoint principal's policy covers. A reader that
// composed `<target prefix>/control-plane` looked somewhere nothing is ever
// written, so every recovery from a target WITH a prefix — which is every real
// one — found no checkpoint at all (found on hardware 2026-09-28: "there is no
// checkpoint object under demo-a/control-plane/manifest.json").
//
// THERE IS ONE DEFINITION OF THE LOCATION AND THIS FUNCTION CALLS IT. The
// constant is the writer's; nobody here spells the directory out again.
func (s *Session) checkpointPrefix() string {
	return strings.Trim(backup.ControlPlanePrefix, "/")
}

// stageRecoveryControlPlane installs the control-plane chart with the kit's
// authority and key material, and with the backend HELD.
//
// THE KIT'S CA IS THE ISSUER, and that is what lets every CLI and agent that
// already pinned it verify the restored control plane with no trust rollover: a
// control plane restored under a different authority is refused by those CLIs
// with an unknown-authority error, and that check is live rather than assumed.
//
// THE BACKEND IS HELD because the restore has not happened yet.
func stageRecoveryControlPlane(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	if s.recoverySel == nil {
		return errors.New("the control-plane stage has no recovery set: it runs after the select stage")
	}
	kitSecrets, err := s.recoverySel.Kit.Secrets(s.Opts.Recovery.FleetKey)
	if err != nil {
		return fmt.Errorf("opening the control-plane kit: %w", err)
	}
	sec, err := controlplane.RecoverySecrets(kitSecrets)
	if err != nil {
		return err
	}
	// A RESUME ADOPTS THE FIELDS THE PREVIOUS ATTEMPT GENERATED. The kit-derived
	// keys must match; the user signing key, the database password and the
	// bootstrap administrator password were generated by that attempt, the
	// Postgres pod has already initialised its volume with its password, and
	// writing new ones would be a different database rather than the same one.
	sec, err = controlplane.EnsureRecoverySecrets(ctx, server, sec, s.recoveryOwnsHost())
	if err != nil {
		return err
	}
	// The checkpoint CronJob is what keeps the rebuilt control plane
	// recoverable from tomorrow, so it points at the bucket the checkpoints
	// came out of and seals to the fleet recipient this recovery holds.
	checkpoint, err := s.checkpointTargetForRecovery(ctx, server)
	if err != nil {
		return err
	}
	values, err := controlplane.Values(controlplane.Settings{
		Domain:     s.Opts.Domain,
		AdminEmail: s.Opts.AdminEmail,
		Checkpoint: checkpoint,
	}, sec)
	if err != nil {
		return err
	}
	held, err := controlplane.HoldBackend(values)
	if err != nil {
		return err
	}
	if _, err := controlplane.Apply(ctx, server, held); err != nil {
		return err
	}
	// The management cluster's own operator trusts the authority in the kit,
	// which is the authority every CLI and agent in the fleet already pinned.
	s.Opts.ControlPlaneCA = []byte(sec.CABundle())
	s.recoveryValues = values
	s.Logf("  the control plane chart was applied with the kit's CA and key material; the backend is held at zero replicas until the checkpoint is loaded")
	return nil
}

// stageRecoveryCheckpoint loads the checkpoint into the held database, migrates
// only if the chart is newer, and starts the backend.
func stageRecoveryCheckpoint(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	if s.recoveryCheckpoint == nil || len(s.recoveryCheckpointDump) == 0 {
		return errors.New("the recovery pre-flight did not select a checkpoint, so there is nothing to load into the database")
	}
	// THE DATABASE POD IS WAITED FOR, IN THIS ONE PLACE, AND NOT IN THE STAGE
	// THAT APPLIED THE CHART. `recovery-control-plane` writes the HelmChart and
	// returns; k3s's Helm controller installs the release afterwards, so at this
	// point the pod may be seconds from existing. Waiting here rather than there
	// is the single place to wait: the chart apply is deliberately HOLDING the
	// backend, so a readiness wait over the whole release would wait for
	// something that will not be ready until this stage starts it, while the
	// database is a dependency of exactly what this stage does next.
	//
	// A RESUME RE-RUNS THIS STAGE, which is why the wait is inside it: the
	// journal skips completed stages, this one failed, and on the second attempt
	// the pod is there and the wait returns immediately.
	deadline, err := s.Bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return fmt.Errorf("the bundle declares no component-ready timeout, so waiting for the database pod has no deadline and this stage will not guess one: %w", err)
	}
	if err := s.waitForDatabasePod(ctx, server, deadline, 5*time.Second); err != nil {
		return err
	}
	// A RESUME DOES NOT LOAD THE CHECKPOINT TWICE. `pg_restore --exit-on-error`
	// into a database that already has the schema fails at its first table, so
	// a run that loaded the checkpoint and then failed before starting the
	// backend would never get past this point again.
	loaded, detail, err := controlplane.DatabaseHasControlPlaneData(ctx, server)
	if err != nil {
		return err
	}
	if loaded {
		s.Logf("  the checkpoint was already loaded by an earlier attempt (%s): not loading it again over its own rows", detail)
	} else {
		s.Logf("  the database is empty (%s): loading %s", detail, s.recoveryCheckpoint.Key)
		if err := controlplane.RestoreCheckpoint(ctx, server, s.recoveryCheckpoint.DumpKey, s.recoveryCheckpointDump, s.Opts.Recovery.FleetKey, s.Reporter); err != nil {
			return err
		}
		s.Logf("  checkpoint %s loaded into the held database", s.recoveryCheckpoint.Key)
	}

	revision, migrated, err := controlplane.MigrateIfNewer(ctx, server, s.recoveryValues, s.Bundle,
		controlplane.ControlPlaneVersion{Version: s.recoveryCheckpoint.ControlPlaneVersion}, s.Reporter)
	if err != nil {
		return err
	}
	if migrated {
		s.Logf("  the chart is newer than the checkpoint: the migration Job ran, and the control plane is at %s", revision)
	} else {
		s.Logf("  the chart is the checkpoint's own version, so no migration ran: the schema in the dump is already this code's")
	}
	s.Record.ControlPlaneMigration = controlplane.MigrationJobName + "@" + revision
	if err := s.saveRecord(); err != nil {
		return err
	}

	started, err := controlplane.StartBackend(s.recoveryValues, 1)
	if err != nil {
		return err
	}
	revision, err = controlplane.Apply(ctx, server, started)
	if err != nil {
		return err
	}
	if err := controlplane.WaitReady(ctx, server, revision, s.Bundle, s.Reporter); err != nil {
		return err
	}
	// The CLI reaches its own control plane through the node, exactly as a
	// first install does: DNS for api.<domain> is not this machine's to have
	// yet, and the backend is a ClusterIP only the node can route to.
	client, err := s.openControlPlaneTunnel(ctx, server)
	if err != nil {
		return err
	}
	s.API = client
	addr, err := controlplane.BackendAddr(ctx, server)
	if err != nil {
		return err
	}
	s.Logf("  the restored control plane is serving through %s (revision %s)", addr, revision)
	return nil
}

// openControlPlaneTunnel returns a control-plane client that reaches the
// backend through the SSH connection this install already holds, signed in with
// the operator's administrator account.
//
// THE ADMINISTRATOR'S OWN PASSWORD IS REQUIRED, and a freshly generated one is
// not a substitute: the administrator accounts live in the RESTORED database,
// so a password the chart generated at this install opens nothing. What is
// generated fresh is the user SIGNING KEY, which is what ends every session
// issued before the recovery.
func (s *Session) openControlPlaneTunnel(ctx context.Context, server k3s.Runner) (*api.Client, error) {
	tunnel, ok := server.(interface {
		DialTCP(ctx context.Context, addr string) (net.Conn, error)
	})
	if !ok {
		return nil, errors.New("the SSH connection to the server cannot open a tunnel to the restored backend, so the CLI cannot sign in to the control plane it just rebuilt")
	}
	addr, err := controlplane.BackendAddr(ctx, server)
	if err != nil {
		return nil, err
	}
	open := func(opts ...api.Option) (*api.Client, error) {
		dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
			return tunnel.DialTCP(ctx, addr)
		}
		return api.New("http://"+addr, append([]api.Option{api.WithDialContext(dial)}, opts...)...)
	}
	base := "https://api." + s.Opts.Domain
	token, err := s.controlPlaneToken(ctx, open, base, s.Opts.AdminPassword)
	if err != nil {
		return nil, err
	}
	return open(api.WithToken(token))
}

// recoveryOwnsHost answers whether THIS OPERATION built the cluster it is
// installing into, which is the ownership test the install Secret needs.
//
// IT IS `StageK3sServer`, NOT THIS STAGE'S OWN COMPLETION. A stage that fails
// in a later attempt clears its completion, so a run that completed this stage
// and then failed it twice on its own bugs would be told its own Secret
// belonged to somebody else — which is exactly what happened on hardware on
// 2026-09-28. The journal's record of having installed k3s under this same
// journal is the durable one: a first run cannot reach this stage on a host
// that already runs Kubernetes, because preflight refuses existing Kubernetes
// unless this journal is the run that put it there (stagePreflight's
// ExistingK3sIsOurs asks the same question for the same reason).
func (s *Session) recoveryOwnsHost() bool {
	_, built := s.Jnl.Completed(StageK3sServer)
	return built
}

// waitForDatabasePod waits until the control plane's database pod exists and is
// Ready, bounded by the deadline its caller took from the bundle manifest, and
// reports what the pod is doing while it waits.
func (s *Session) waitForDatabasePod(ctx context.Context, r k3s.Runner, deadline, interval time.Duration) error {
	if deadline <= 0 {
		return errors.New("waiting for the database pod was given no deadline: every wait in this CLI is bounded by the bundle's limits, and a default here would be an unbounded one")
	}
	// WAIT'S RESULT IS THE VERDICT, and its error is only the transport. A
	// deadline that expires returns a Fail with a nil error, so a caller that
	// checked only `err` would carry on with an observation it had just
	// refused — which is what this stage did before the hardware run.
	result, err := converge.Wait(ctx, func(ctx context.Context) (bool, converge.State, error) {
		return controlplane.DatabasePodState(ctx, r)
	}, converge.Options{
		Name:     "control-plane database",
		Deadline: deadline,
		Interval: interval,
		Reporter: s.Reporter,
	})
	if err != nil {
		return fmt.Errorf("waiting for the control plane's database pod before restoring the checkpoint into it: %w", err)
	}
	if err := result.Err(); err != nil {
		return fmt.Errorf("the control plane's database is not serving, so the checkpoint is not loaded into it: %w. The chart is applied and its Postgres is not Ready; a resume re-runs this stage and waits again", err)
	}
	return nil
}

// stageRecoveryProvisional compares the desired state the restored database
// holds with what surviving clusters report, and SHOWS the differences.
//
// IT PUSHES NOTHING. A checkpoint is a moment, and a cluster that kept running
// after it has moved on: writing the checkpoint's values back at it would revert
// work done after the checkpoint was taken — the recovery would damage the very
// fleet it was recovering. The operator decides.
func stageRecoveryProvisional(ctx context.Context, s *Session) error {
	client, err := s.recoveryAPI()
	if err != nil {
		return err
	}
	restored, err := s.desiredState(ctx, client)
	if err != nil {
		return err
	}
	deadline, err := s.Bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return fmt.Errorf("the bundle declares no component-ready timeout: %w", err)
	}
	s.Logf("  comparing the restored desired state with what surviving clusters report (up to %s)", deadline)
	live, expired := s.waitForClustersToReport(ctx, client, deadline)
	diffs := recovery.CompareProvisional(restored, live)
	recovery.Report(s.Out, diffs)
	if expired != nil {
		s.Logf("  note: some clusters had not reported when this wait ended (%v). A cluster that has not reported yet is not a difference; `kubenest health` reports it once it reconnects", expired)
	}
	if len(diffs) > 0 {
		s.Logf("  the restored desired state is PROVISIONAL: nothing here is pushed back at a cluster that moved on. Decide from the records above, then re-apply what the checkpoint may have missed — for a revoked agent token, `kubenest cluster rotate-token` refuses it for good")
	}
	return nil
}

// desiredState reads the records the comparison is about: every cluster's
// inventory revision and its recorded bundle.
func (s *Session) desiredState(ctx context.Context, client *api.Client) (recovery.DesiredState, error) {
	out := recovery.DesiredState{
		Inventories: map[string]recovery.Fact{},
		Bundles:     map[string]recovery.Fact{},
	}
	orgs, err := client.ListOrgs(ctx)
	if err != nil {
		return out, err
	}
	for _, org := range orgs {
		clusters, err := client.ListOrgClusters(ctx, org.ID)
		if err != nil {
			return out, err
		}
		for _, cluster := range clusters {
			record, err := client.BundleRecord(ctx, cluster.ID)
			if err != nil {
				// A cluster with no record has nothing to compare, which is a
				// fact rather than a failure.
				continue
			}
			out.Inventories[cluster.ID] = recovery.Fact{Value: fmt.Sprintf("revision %d, %d host(s)", record.Revision, len(record.Hosts))}
			out.Bundles[cluster.ID] = recovery.Fact{Value: record.BundleVersion}
		}
	}
	return out, nil
}

// waitForClustersToReport reads the same records again until the deadline
// passes. It returns what it last read, and a non-nil error when the deadline
// expired first.
func (s *Session) waitForClustersToReport(ctx context.Context, client *api.Client, deadline time.Duration) (recovery.DesiredState, error) {
	stop := time.Now().Add(deadline)
	var last recovery.DesiredState
	for {
		current, err := s.desiredState(ctx, client)
		if err != nil {
			return last, err
		}
		last = current
		if !time.Now().Before(stop) {
			return last, fmt.Errorf("the wait for clusters to report ended after %s", deadline)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// checkpointTargetForRecovery builds the chart's checkpoint group for a
// rebuild: the same bucket and prefix the checkpoints came out of, sealed to
// the fleet recipient this recovery holds.
//
// The control plane's own principal is read from its own variables, never from
// the cluster's KUBENEST_BACKUP_* pair: the checkpoints and a cluster's backups
// must not share a credential (PLAN 7.8).
func (s *Session) checkpointTargetForRecovery(ctx context.Context, server k3s.Runner) (*controlplane.CheckpointTarget, error) {
	target, err := s.recoveryTarget()
	if err != nil {
		return nil, err
	}
	keyID, secret := checkpointCredentials()
	cp := controlplane.NewCheckpointTarget(target, s.recoveryRecipient, keyID, secret)
	if err := controlplane.EnsureCredentials(ctx, server, cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

// recoveryFleetRecipient is the recipient every kit this recovery writes is
// sealed to: the public half of the fleet key this process holds.
func (s *Session) recoveryFleetRecipient() string {
	if s.recoveryRecipient != "" {
		return s.recoveryRecipient
	}
	return s.fleetRecipient()
}
