// Recovery: the install paths that rebuild a cluster from its bucket.
//
// Two shapes share this file, because they share the first three steps and
// differ in what they recover:
//
//	S6 (T4.9)  `kubenest platform install --cluster <id> --restore-from latest
//	           --recovery-kit s3` rebuilds a single-server WORKLOAD cluster
//	           under its own identity and restores its namespaces.
//	S11 (T4.8) `kubenest platform install --control-plane --restore-from latest
//	           --recovery-kit s3` rebuilds an all-in-one host: a fresh
//	           management cluster, the control plane's newest eligible
//	           checkpoint restored into it, and then the management cluster's
//	           own workload namespaces.
//
// Nothing from the dead host's datastore is replayed on either path. The
// workload cluster's identity is adopted from the control plane by its
// immutable id; the control plane's own identity is the kit's CA, which is what
// lets every CLI and agent that already pinned it verify the new host without a
// trust rollover.
package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/recovery"
	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/register"
	"kubenest.io/cli/pkg/stages"
)

// stageBinder wires one stage function to a session. It is the type Plan's
// closure has, named so the control-plane recovery's plan can be built in
// another file of this package.
type stageBinder func(func(context.Context, *Session) error) stages.StageFunc

// The recovery stages. They are named in the journal and on the wire like every
// other stage, and their order is the six steps of PLAN 7.9.
const (
	// stageRecoverySelect reads the recovery set for the cluster's immutable id
	// and logs what it selected. It writes nothing.
	StageRecoverySelect = "recovery-select"
	// StageRecoveryPreflight is the recovery's own pre-flight: the four check
	// answers, the operator's fencing confirmation, and the replacement host's
	// capacity against the volumes about to be restored. It writes nothing
	// anywhere, which is what makes abandoning here free.
	StageRecoveryPreflight = "recovery-preflight"
	// StageRecoveryOwnership takes recovery ownership in the bucket and records
	// it in the operation, before the first destructive action.
	StageRecoveryOwnership = "recovery-ownership"
	// StageRecoveryRepository opens the EXISTING Velero repository the recovery
	// set names, with the kit's password, before Velero starts. It never
	// initialises a repository.
	StageRecoveryRepository = "recovery-repository"
	// StageRecoveryRestore restores every namespace the recovery set's backup
	// covers.
	StageRecoveryRestore = "recovery-restore"
	// StageRecoveryActivate releases the projects recovery mode held, after
	// their data is back.
	StageRecoveryActivate = "recovery-activate"
)

// RecoveryOptions is a recovery install's request, resolved from the flag
// surface.
type RecoveryOptions struct {
	// ClusterID is --cluster: the cluster's IMMUTABLE id. A display name never
	// authorises adoption, so nothing else may select the cluster.
	ClusterID string
	// RestoreFrom is --restore-from. "latest" is the only value today, and it
	// is required: a recovery names what it starts from.
	RestoreFrom string
	// Kit is --recovery-kit: where the kit comes from. "s3" is the only value
	// today.
	Kit string
	// FleetKey is the private fleet recovery key, read from a file. It is held
	// in this process only: it is never journalled, logged or written to a host.
	FleetKey string
	// Backup optionally names one backup instead of the set's newest.
	Backup string
	// OldHostFenced is the operator's explicit confirmation that the machine
	// being replaced cannot come back — powered off, or fenced at the provider.
	// It is a recorded step, not an assumption: an old host that rejoins is two
	// clusters with one identity.
	OldHostFenced bool
	// AcknowledgeSingleOperator is the F20 acknowledgement, required only when
	// the target cannot create an object conditionally (probe P3 question 1).
	AcknowledgeSingleOperator bool
	// Kind is which authority the kit belongs to.
	Kind recoverykit.Kind
}

// Validate refuses a recovery request that cannot be honoured, before anything
// is read from anywhere.
func (r *RecoveryOptions) Validate() error {
	if r.ClusterID == "" && r.Kind == recoverykit.KindCluster {
		return errors.New("--restore-from needs --cluster <cluster-id>: a recovery adopts a cluster by its IMMUTABLE id, and a display name never authorises adoption. Take the id from the recovery set's binding (or `kubenest cluster list`)")
	}
	if r.RestoreFrom == "" {
		return errors.New("--recovery-kit needs --restore-from: name what the recovery starts from (today: latest)")
	}
	if r.RestoreFrom != "latest" {
		return fmt.Errorf("--restore-from %q is not something this release recovers from: the only value is latest, which is the newest eligible recovery set for the cluster", r.RestoreFrom)
	}
	if r.Kit != "s3" {
		return fmt.Errorf("--recovery-kit %q is not a kit source this release has: the only value is s3, the bucket the recovery sets and kits live in", r.Kit)
	}
	if r.Kind == recoverykit.KindCluster && r.ClusterID == "" {
		return errors.New("a cluster recovery needs the cluster's immutable id")
	}
	if strings.TrimSpace(r.FleetKey) == "" {
		return errors.New("a recovery needs the fleet recovery key: without it the kit cannot be opened, and without the kit there is no repository password and no join token. Pass --fleet-key-file (the AGE-SECRET-KEY-1... the control-plane install printed once)")
	}
	return nil
}

// RecoveryMode reports whether this install is a recovery.
func (o Options) RecoveryMode() bool { return o.Recovery != nil }

// recoveryName is what messages call the cluster: the display name once the
// adoption has read it, and the immutable id until then. It is never used to
// select anything.
func (s *Session) recoveryName() string {
	if s.Opts.Name != "" {
		return s.Opts.Name
	}
	if s.Opts.Recovery != nil {
		return s.Opts.Recovery.ClusterID
	}
	return s.Jnl.ClusterID
}

// recoveryAPI is the control plane a recovery adopts the cluster through. A
// recovery always has one: the cluster's record, its projects and its
// incarnation live there, and reading them is what "adopt by immutable id"
// means. It is deliberately NOT the version-checked client: recovery checks
// compatibility against the recovery set's manifest, not against a live
// version endpoint (PLAN 7.8).
func (s *Session) recoveryAPI() (*api.Client, error) {
	if s.API == nil {
		return nil, errors.New("this recovery has no control-plane client: adopting a cluster by its immutable id, and registering the new incarnation, both happen on the control plane. Run `kubenest login` on this machine first")
	}
	return s.API, nil
}

// recoveryTarget parses --backup-target into the bucket a recovery reads. It is
// required: the recovery set, the kit and the backups all live in a bucket
// whose coordinates nothing else knows, and a fresh laptop has no local kit to
// read them from.
func (s *Session) recoveryTarget() (backup.Target, error) {
	if strings.TrimSpace(s.Opts.BackupTarget) == "" {
		return backup.Target{}, errors.New("a recovery needs --backup-target: it is the only thing that says which bucket holds this cluster's recovery set and backup, and a machine that never ran the install has no local copy to fall back on. S3 credentials come from KUBENEST_BACKUP_ACCESS_KEY_ID / KUBENEST_BACKUP_SECRET_ACCESS_KEY")
	}
	return parseBackupTarget(s.Opts.BackupTarget)
}

// stageRecoverySelect selects the recovery set by the cluster's immutable id
// and keeps it for every later stage (PLAN 7.9 step 2).
//
// IT READS AND NOTHING ELSE. The four questions the set has to answer are asked
// in the pre-flight, which is where a refusal costs nothing; this stage's job is
// to find the artifact, and to get the cluster's display name and organisation
// from the record the id names.
func stageRecoverySelect(ctx context.Context, s *Session) error {
	rec := s.Opts.Recovery
	if rec == nil {
		return errors.New("the recovery-selection stage ran on an install that is not a recovery")
	}
	target, err := s.recoveryTarget()
	if err != nil {
		return err
	}
	// A RESUMED RUN KEEPS THE STORE IT ALREADY READ THE SET FROM, so the set it
	// selected is still the one the operator was shown rather than whatever the
	// bucket holds by the time the run resumes.
	store := s.recoveryStore
	if store == nil {
		client, err := target.S3Client()
		if err != nil {
			return err
		}
		store = client
	}
	apiClient, err := s.recoveryAPI()
	if err != nil {
		return err
	}

	// ADOPTING BY ID IS THE FIRST THING THAT HAPPENS, and it is what a display
	// name can never do: the record is read by the immutable id, and the name
	// it carries is used for messages and nothing else.
	cluster, err := apiClient.GetCluster(ctx, rec.ClusterID)
	if err != nil {
		return fmt.Errorf("adopting cluster %s by its immutable id: %w. If this id is right, the control plane is the thing to check; if it is a display name, note that a name never authorises adoption and the id is in the recovery set's binding", rec.ClusterID, err)
	}
	if cluster.Status == "deleted" {
		return fmt.Errorf("cluster %s (%s) is deleted on the control plane: its record is gone, so there is nothing to adopt. Recovery rebuilds a cluster that still exists as a record; a cluster that was deleted needs a new install", rec.ClusterID, cluster.Name)
	}
	s.Opts.Name = cluster.Name
	s.orgID = cluster.OrgID
	s.Jnl.ClusterID = cluster.ID
	if err := s.saveRecord(); err != nil {
		return err
	}

	expected := recoverykit.Binding{
		Kind:           rec.Kind,
		InstanceID:     s.instanceIdentity(),
		OrganisationID: cluster.OrgID,
		ClusterID:      cluster.ID,
	}
	sel, err := recovery.Select(ctx, store, recovery.Scope(recoverykit.Location{Prefix: target.Prefix}), recovery.Request{
		Kind:      rec.Kind,
		ClusterID: cluster.ID,
		Backup:    rec.Backup,
		Expected:  expected,
	})
	if err != nil {
		return err
	}
	if err := checkRecordedLocation(sel.Set.S3Location, target); err != nil {
		return err
	}

	s.recoveryStore = store
	s.recoveryTargetValue = target
	s.recoverySel = sel
	// The kit the bucket holds IS the kit in place for this install. It is what
	// lets the backup-target stage configure Velero without writing a new kit
	// over a recovery — a new kit is a new artifact, and the set this recovery
	// selected must keep meaning what it means.
	s.kit = sel.Kit

	s.Logf("  recovery: cluster %s (%s) adopts %s kit artifact %s; the newest eligible backup is %q, completed %s",
		cluster.Name, cluster.ID, rec.Kind, sel.Set.ArtifactID, sel.Backup.Name, sel.Backup.CompletedAt.UTC().Format(time.RFC3339))
	s.Logf("  recovery: the set at %s records repository %s", sel.SetKey, sel.Set.VeleroRepositoryID)
	return nil
}

// checkRecordedLocation refuses a recovery whose bucket is not the one the
// artifact records. Restoring from a different bucket than the set names is not
// the recovery the set describes, and the set is the only thing that binds an
// identity to a backup.
func checkRecordedLocation(loc recoverykit.Location, target backup.Target) error {
	if got, want := backup.EndpointURL(loc.Endpoint), backup.EndpointURL(target.Endpoint); !strings.EqualFold(got, want) {
		return fmt.Errorf("the recovery set records its bucket at %s and this run was pointed at %s: recovering from a different store than the set names is not this set's recovery", got, want)
	}
	if loc.Bucket != target.Bucket {
		return fmt.Errorf("the recovery set records bucket %q and this run was pointed at bucket %q", loc.Bucket, target.Bucket)
	}
	if loc.Region != target.Region {
		return fmt.Errorf("the recovery set records region %q and this run was pointed at region %q", loc.Region, target.Region)
	}
	if strings.Trim(loc.Prefix, "/") != strings.Trim(target.Prefix, "/") {
		return fmt.Errorf("the recovery set records prefix %q and this run was pointed at prefix %q: a cluster's kits, sets and backups all live under its own prefix, and reading one cluster's set while writing into another's prefix is how a recovery lands in the wrong place", loc.Prefix, target.Prefix)
	}
	return nil
}

// stageRecoveryPreflight is the recovery's own pre-flight, after the bundle's
// eleven checks.
//
// NOTHING IS WRITTEN HERE, anywhere: not to a machine, not to the bucket. That
// is what makes a refusal here free, and all four refusals below are ones the
// operator must see before a single byte changes.
func stageRecoveryPreflight(ctx context.Context, s *Session) error {
	rec := s.Opts.Recovery
	if rec == nil {
		return errors.New("the recovery pre-flight ran on an install that is not a recovery")
	}
	if s.recoverySel == nil || s.recoveryStore == nil {
		return errors.New("no recovery set was selected: the select stage runs first, and a recovery with no set has nothing to check")
	}

	// Step 2: the four answers, separately, so a failure names which one.
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
		{"recovery set complete and about this cluster", answers.Set},
	} {
		mark := "no "
		if answer.a.OK {
			mark = "yes"
		}
		s.Logf("  recovery check [%s] %s: %s", mark, answer.what, answer.a.Detail)
	}
	if !answers.AllGood() {
		return fmt.Errorf("this cluster's recovery set is not usable: %s. Nothing has been changed anywhere — fix what the failing line names and run the same command again", strings.Join(answers.Failed(), "; "))
	}
	s.Logf("  recovery check: only a completed restore proves restoration; these answers say the artifact is present, readable and about the right cluster")

	// Step 1's other half: the operator's confirmation that the old host cannot
	// come back. It is asked here, before ownership, because an old host that
	// rejoins after this point is a second cluster answering to one identity.
	if !rec.OldHostFenced {
		return errors.New("recovery replaces a machine, so it needs the operator to confirm the old one is FENCED first: power it off at the provider, or make it unreachable, then pass --old-host-fenced. Nothing has been changed. An unfenced old host that rejoins after the new one registers is two clusters behind one identity, and every operation after that is against whichever answered")
	}

	// The replacement host's capacity, against what the recovery set says the
	// restored volumes need. Refused here, where it is a two-minute fix.
	for _, node := range s.NodesWithRole(RoleServer) {
		capacity, err := recovery.CheckCapacity(ctx, node.Runner, s.recoverySel.Set, s.Opts.StorageDevice)
		s.Logf("  recovery check [%s] capacity on %s: %s", yesNo(err == nil || capacity.Known), node.Address, capacity.Report())
		if err != nil {
			return err
		}
	}
	return nil
}

func yesNo(ok bool) string {
	if ok {
		return "yes"
	}
	return "no "
}

// stageRecoveryOwnership takes recovery ownership in the bucket and records it
// in the operation BEFORE the first destructive action (PLAN 7.2, 7.9 step 1).
//
// THE CLUSTER-SIDE LOCK IS GONE, which is the whole reason this exists: the
// operation ConfigMap lives in kube-system on a host that no longer exists. What
// is left is the bucket, so ownership is an object there — created
// conditionally, so exactly one laptop can hold it, and never released by
// elapsed time.
func stageRecoveryOwnership(ctx context.Context, s *Session) error {
	rec := s.Opts.Recovery
	if rec == nil {
		return errors.New("the ownership stage ran on an install that is not a recovery")
	}
	if s.recoveryTargetValue.Endpoint == "" {
		return errors.New("the ownership stage has no parsed backup target")
	}
	key, err := recoverykit.ParseFleetKey(rec.FleetKey)
	if err != nil {
		return fmt.Errorf("reading the fleet recovery key this recovery holds: %w", err)
	}
	// A RESUMED RUN KEEPS THE COPY IT ALREADY BUILT, so it claims the same
	// ownership object rather than rebuilding a client it does not need.
	claim := s.recoveryCopy
	if claim == nil {
		built, err := s.recoveryTargetValue.OperationCopy(key.Recipient())
		if err != nil {
			return err
		}
		claim = &built
	}

	now := time.Now().UTC()
	opID := operation.NewOperationID()
	record := &operation.Record{
		OperationID: opID,
		Request: operation.Request{
			Kind:    operation.KindHostRecovery,
			Cluster: s.recoveryName(),
			Versions: map[string]string{
				"recovery-set": s.recoverySel.SetKey,
				"backup":       s.recoverySel.Backup.Name,
			},
		},
		Executor: operation.Executor{
			Token:     operation.NewToken(),
			Operator:  recoveryOperator(),
			State:     operation.ExecutorRunning,
			Heartbeat: now,
		},
		Stage:     StageRecoveryOwnership,
		StartedAt: now,
		UpdatedAt: now,
	}
	// The off-cluster copy is written FIRST: it is the only record that can
	// survive the operation, and a claim without a record would be an object in
	// a bucket naming an operation nobody can read.
	recordKey, err := claim.Write(ctx, record)
	if err != nil {
		return err
	}
	s.recoveryOpID = opID
	ownerKey, mode, err := recovery.Claim(ctx, *claim, opID, recoveryOperator(), now, rec.AcknowledgeSingleOperator)
	if err != nil {
		return err
	}
	s.recoveryOwnerKey = ownerKey
	s.Logf("  recovery ownership [%s]: %s", mode, ownerKey)
	s.Logf("  recovery operation %s recorded at %s before any change", opID, recordKey)
	if mode == recovery.OwnershipAcknowledged {
		s.Logf("  warning: this target cannot create an object conditionally, so ownership does not exclude a second operator. The acknowledgement is recorded in %s; do not start a second recovery of this cluster", ownerKey)
	}
	return nil
}

// recoveryOperator names the person and machine holding the recovery, in the
// operator's own words. It is the same shape pkg/upgrade computes for its own
// records; it is computed here rather than imported because pkg/upgrade's tests
// import this package and the import would close a cycle.
func recoveryOperator() string {
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		if user == "" {
			return "someone@somewhere"
		}
		return user
	}
	if user == "" {
		return host
	}
	return user + "@" + host
}

// stageRecoveryRepository opens the existing Velero repository the recovery set
// names, with the kit's password, BEFORE Velero starts.
//
// THE ORDER IS THE POINT (probe P3 question 2). Velero's own key setup creates
// the repository-credentials Secret holding a vendored default when it is
// absent, and a kopia repository created under that password keeps it for its
// whole life. So the kit's password must be on the cluster before the Velero
// server is: this stage runs before the stage that installs Velero.
func stageRecoveryRepository(ctx context.Context, s *Session) error {
	sel := s.workloadSelection()
	if sel == nil {
		return errors.New("the repository stage has no recovery set: it runs after the select stage")
	}
	server, err := s.Server()
	if err != nil {
		return err
	}
	secrets, err := sel.Kit.Secrets(s.Opts.Recovery.FleetKey)
	if err != nil {
		return fmt.Errorf("opening the recovery kit for the repository password: %w", err)
	}
	password, ok := secrets[recoverykit.KeyVeleroRepoPassword]
	if !ok || password == "" {
		return fmt.Errorf("the recovery kit carries no %s, so there is no password that opens the repository the recovery set names. A kit without it cannot open an existing repository, and generating one would create a second repository beside the data", recoverykit.KeyVeleroRepoPassword)
	}
	repoID := sel.Set.VeleroRepositoryID
	if repoID == "" {
		repoID = sel.Kit.VeleroRepositoryID
	}
	state, err := recovery.OpenRepository(ctx, server, recovery.Repository{ID: repoID, Password: password})
	if err != nil {
		return err
	}
	switch {
	case state.RepositoryID != "":
		s.Logf("  repository %s: opened with the kit's password; the object was already on the cluster and was left exactly as it was", state.RepositoryID)
	case state.Existed():
		s.Logf("  repository %s: no BackupRepository object exists yet, which is the rebuilt-cluster case: Velero creates it the first time it connects, against the repository this password opens", repoID)
	default:
		s.Logf("  repository %s: the repository-credentials Secret did not exist on this fresh host and was created from the kit, before Velero starts; nothing was initialised", repoID)
	}
	return nil
}

// stageRecoveryRegister adopts the cluster the immutable id names as a NEW
// physical incarnation and mints the credentials the rebuilt agent will carry
// (PLAN 7.9 step 4).
//
// THREE THINGS HAPPEN, IN THIS ORDER, AND EACH IS LOAD-BEARING:
//
//  1. the cluster record the immutable id names is confirmed to be the one the
//     recovery set is bound to. The org in the binding and the org on the
//     record must agree; if they do not, the set belongs to another cluster and
//     nothing is registered;
//  2. a new incarnation is recorded against the cluster. The identity is the
//     CLUSTER's, never the host's: the machine is new, the cluster is the same
//     one, and the control plane has to be able to say which machine is which;
//  3. fresh credentials are minted. A re-mint is what retires the superseded
//     agent token — it raises the cluster's revocation floor to the new version
//     and pushes it to the hub — so the credential the dead host's disk held
//     stops working instead of staying valid for its remaining year
//     (kn-t49-recovering-lost-single-server-uzka.1, probe P3 question 4).
//
// Nothing here is replayed from the old host: the cluster's projects, members
// and windows are the control plane's, and they were never on the dead machine.
func stageRecoveryRegister(ctx context.Context, s *Session) error {
	if s.recoverySel == nil {
		return errors.New("the register stage has no recovery set: it runs after the select stage")
	}
	client, err := s.recoveryAPI()
	if err != nil {
		return err
	}
	cluster, err := client.GetCluster(ctx, s.Jnl.ClusterID)
	if err != nil {
		return fmt.Errorf("reading cluster %s to adopt it: %w", s.Jnl.ClusterID, err)
	}
	if want := s.recoverySel.Set.Binding.OrganisationID; want != "" && cluster.OrgID != want {
		return fmt.Errorf("cluster %s is in organisation %s and the recovery set at %s is bound to organisation %s: this set belongs to another cluster, and adopting this one with it would restore another cluster's data here. Nothing has been registered",
			cluster.ID, cluster.OrgID, s.recoverySel.SetKey, want)
	}
	s.Opts.Name = cluster.Name
	s.orgID = cluster.OrgID
	s.Record.Adopted = true

	incarnation, err := client.RecordIncarnation(ctx, cluster.ID, "recovery",
		fmt.Sprintf("rebuilt from recovery set %s on host %s", s.recoverySel.SetKey, s.Opts.Servers[0]))
	if err != nil {
		return fmt.Errorf("recording the new physical incarnation of cluster %s: %w", cluster.ID, err)
	}
	s.Logf("  incarnation %d recorded for cluster %s", incarnation.Ordinal, cluster.ID)

	// The bundle this REBUILD installs travels with the mint: the backend
	// resolves the operator chart ref from the bundle an install declares, and
	// a recovery that named nothing would pull the newest released bundle
	// instead of the one the recovery set was written for (kn-t72).
	creds, err := register.MintCredentials(ctx, client, cluster.ID, s.recoveryBundleVersion())
	if err != nil {
		return err
	}
	s.Creds = creds
	s.Record.TokenVersion = creds.AgentJWT.TokenVersion
	if creds.RepoCredential != nil {
		s.Record.RepoURL = creds.RepoCredential.RepoURL
	}
	s.Logf("  agent credentials minted at token version %d; the superseded incarnation's token is refused at the hub", creds.AgentJWT.TokenVersion)
	return s.saveRecord()
}

// workloadSelection is the set whose backups this recovery restores: for a
// workload cluster its own, and for a control-plane recovery the management
// cluster's, which is written beside the instance's control-plane set because
// the control plane runs in a cluster.
func (s *Session) workloadSelection() *recovery.Selection {
	if s.recoveryWorkloadSel != nil {
		return s.recoveryWorkloadSel
	}
	return s.recoverySel
}

// recoveryBundleVersion is the bundle this rebuild installs: the one the
// recovery set names, when it names one, and the one the command was given
// otherwise. The set is the authority when it disagrees: it is the manifest
// that was current when the backup was taken.
func (s *Session) recoveryBundleVersion() string {
	if s.recoverySel != nil {
		if v := s.recoverySel.Set.Versions["bundle"]; v != "" {
			return v
		}
	}
	return s.Opts.Bundle
}

// stageRecoveryRestore restores every namespace the recovery set's backup
// covers (PLAN 7.9 step 5).
//
// THE RESTORE COMMAND IS pkg/backup's, unchanged: the same eligibility
// judgement, the same pause on each project, the same refusal to activate
// anything. What this stage adds is the list of namespaces — the recovery set's
// backup records its coverage — and the sequencing that keeps Job, CronJob and
// application containers from running before their data is back: the restore
// leaves every project paused, and the activate stage releases them after.
func stageRecoveryRestore(ctx context.Context, s *Session) error {
	sel := s.workloadSelection()
	if sel == nil {
		return errors.New("the restore stage has no recovery set: it runs after the select stage")
	}
	server, err := s.Server()
	if err != nil {
		return err
	}
	namespaces := sel.Backup.Coverage
	if len(namespaces) == 0 {
		return fmt.Errorf("the recovery set's backup %q records no namespace coverage, so there is nothing this stage could restore. A backup whose expected set was never recorded cannot be shown to cover anything — restore the workload by hand with `kubenest backup restore --namespace <ns> --from %s`, or select a backup that records what it covers", sel.Backup.Name, sel.Backup.Name)
	}
	deps := backup.RestoreDeps{
		Cluster: backup.NewK3sCluster(server),
		Backups: backup.NewVeleroBackups(server),
		Store:   &operation.Store{Runner: server, Operator: recoveryOperator()},
		Runner:  server,
	}
	for _, namespace := range namespaces {
		s.Logf("  restoring namespace %s from %s", namespace, sel.Backup.Name)
		opts := backup.RestoreOptions{
			Cluster:   s.recoveryName(),
			Namespace: namespace,
			From:      sel.Backup.Name,
			// The rebuild has no namespace yet, but --replace is the honest
			// statement: the restore must not refuse because a namespace the
			// desired state already created is in the way.
			Replace:       true,
			Confirm:       true,
			AcceptDataAge: true,
			Bundle:        s.Bundle,
		}
		if err := backup.RunRestore(ctx, s.Out, nil, opts, deps); err != nil {
			return stages.NewComponentError("recovery-restore", fmt.Errorf("restoring namespace %s from backup %s: %w", namespace, sel.Backup.Name, err))
		}
	}
	return nil
}

// stageRecoveryActivate releases the projects recovery mode held, once their
// data is back (PLAN 7.9 step 6).
//
// ACTIVATION IS THE OPERATOR'S POSITIVE DECISION, and in recovery mode nothing
// else releases a project: the operator holds every project until it carries
// the activation annotation, so a pause that is removed is not a release. This
// stage writes that annotation on each restored namespace's project — and only
// after its restore completed, which is what keeps a restored Job, CronJob or
// application container from running against data that is not back.
//
// The project itself is created by the operator from the control plane's
// desired state, so this waits for it, bounded, rather than failing on the
// first look.
func stageRecoveryActivate(ctx context.Context, s *Session) error {
	if s.workloadSelection() == nil {
		return errors.New("the activate stage has no recovery set: it runs after the select stage")
	}
	server, err := s.Server()
	if err != nil {
		return err
	}
	cluster := backup.NewK3sCluster(server)
	deadline, err := s.Bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return fmt.Errorf("the bundle declares no component-ready timeout, so waiting for a project to appear has no deadline and this stage will not guess one: %w", err)
	}
	for _, namespace := range s.workloadSelection().Backup.Coverage {
		ns := namespace
		probe := func(ctx context.Context) (bool, converge.State, error) {
			hold, err := cluster.ProjectHold(ctx, ns)
			if err != nil {
				return false, converge.State{Object: "project " + ns, Detail: err.Error()}, nil
			}
			if hold == nil {
				return false, converge.State{Object: "project " + ns + " in " + backup.ProjectCRNamespace, Status: "absent", Detail: "the operator creates it from the control plane's desired state; recovery mode holds it until this stage activates it"}, nil
			}
			return true, converge.State{Object: "project " + ns, Status: "present"}, nil
		}
		// The RESULT is the verdict and the error is only the transport: a
		// deadline that expires returns a Fail with a nil error, and activating
		// a project whose wait failed is exactly the ordering this stage exists
		// to prevent.
		result, err := converge.Wait(ctx, probe, converge.Options{
			Name:     "project/" + ns,
			Deadline: deadline,
			Reporter: s.Reporter,
		})
		if err != nil {
			return fmt.Errorf("waiting for the project of restored namespace %s: %w", ns, err)
		}
		if err := result.Err(); err != nil {
			return fmt.Errorf("the project of restored namespace %s is not there, so it is not activated: %w. Its data is restored and the cluster is safe; re-running this install resumes here", ns, err)
		}
		if err := cluster.AnnotateProject(ctx, ns, backup.ActivateAnnotationKey, s.recoveryOpID); err != nil {
			return stages.NewComponentError("kubenest-agent", fmt.Errorf("activating project %s: %w", ns, err))
		}
		s.Logf("  activated %s: %s=%s on project %s/%s", ns, backup.ActivateAnnotationKey, s.recoveryOpID, backup.ProjectCRNamespace, ns)
	}
	return nil
}
