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
	"kubenest.io/cli/pkg/k3s"
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

	if err := s.refuseASwitchedSet(sel); err != nil {
		return err
	}
	s.Record.RecoverySetKey = sel.SetKey
	s.Record.RecoveryBackupName = sel.Backup.Name
	s.Record.RecoveryRestoreSkipReason = sel.RestoreSkipReason
	if err := s.saveRecord(); err != nil {
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
		capacity, err := recovery.CheckCapacity(ctx, node.Runner, s.recoverySel.Set, s.deviceFor(node.Address))
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
	// Journalled, because the ownership stage is skippable on a resume and the
	// activate stage needs the operation id as the annotation's value.
	s.Record.RecoveryOperationID = opID
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

	// A RESUME DOES NOT RECORD A SECOND INCARNATION. The operator is already
	// installed and holding credentials from the earlier attempt, so minting
	// again would bump the token version a second time and raise the floor
	// above the token that machine is using — the same reason the ordinary
	// register stage re-mints only when the agent stage is still ahead of it.
	if _, agentInstalled := s.Jnl.Completed(StageAgent); agentInstalled {
		s.Logf("  the incarnation was already recorded and the agent installed by an earlier attempt; not recording another or re-minting")
		return s.saveRecord()
	}

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

// waitForRecoveryBackup waits, bounded, until Velero lists the backup this
// recovery restores.
//
// IT WAITS FOR THE BACKUP AND NOT FOR THE STORAGE LOCATION. A location that is
// Available says Velero can reach the bucket; it does not say Velero has copied
// anything out of it, and on a cluster that was just built the two moments are
// minutes apart (lab s6, 2026-09-28). What the restore needs is the Backup
// object, and the object is what this looks for.
//
// THE READ IS THE SAME ONE THE ELIGIBILITY CHECK IN THE STAGE BELOW MAKES, on
// purpose: a wait with its own idea of what "the backup is on this cluster"
// means could pass the wait and then be told by the check that it is not there,
// which is the failure this wait exists to remove.
//
// An error that is not "no such backup" is an OBSERVATION, not a verdict: an
// API server restarting, or a location Velero is rewriting, must not end a wait
// the deadline would have let recover. Only the deadline ends it, and the
// refusal it produces repeats what the last look found plus what Velero says
// about the location — because "the backup is not on this cluster" is what sent
// a reader on the hardware to look for a backup that was never the problem.
//
// The loop itself is recoveryWait's, shared with the wait for the projects the
// restore pauses: one clock, one bound, one shape.
func (s *Session) waitForRecoveryBackup(ctx context.Context, r k3s.Runner, name, namespace string, deadline time.Duration) error {
	backups := backup.NewVeleroBackups(r)
	// location is what the last look found Velero saying about the storage
	// location, kept between the progress line and the refusal so that one look
	// reads it once.
	var location string
	wait := recoveryWait{
		Name:     fmt.Sprintf("velero to list backup %s in namespace %s", name, backup.Namespace),
		Deadline: deadline,
		Look: func(ctx context.Context) (bool, string, error) {
			_, err := backups.NamedBackup(ctx, namespace, name)
			if err == nil {
				return true, "", nil
			}
			location = s.recoveryStorageLocationReport(ctx, r)
			if errors.Is(err, backup.ErrNoBackup) {
				return false, "velero's backup-sync controller has not listed it yet", nil
			}
			return false, fmt.Sprintf("velero could not be asked for it: %v", err), nil
		},
		// A WAIT THAT PRINTS NOTHING IS A CLI THAT LOOKS HUNG, and this one can
		// run for the bundle's whole component-ready deadline. Every look
		// reports the backup and what Velero currently says about the location
		// it would sync through.
		Report: func(observation string) {
			s.Logf("  waiting for Velero to list backup %s in namespace %s: %s (last look: %s)", name, backup.Namespace, location, observation)
		},
		Refuse: func(observation string) error {
			return fmt.Errorf("velero has not listed backup %s within %s (limits.timeouts.component-ready), so the restore did not start and no namespace was touched: %s; %s. The bucket holds this backup, and velero's backup-sync controller copies a bucket's backups onto a cluster on its own schedule (its default backup-sync period is one minute) and only after the storage location has validated. Nothing was changed by waiting. Run the recovery again once velero lists the backup — a resume re-runs this stage and does not repeat the stages before it — or restore the workload by hand with `kubenest backup restore --namespace %s --from %s --replace --accept-data-age --confirm`", name, deadline, location, observation, namespace, name)
		},
	}
	if err := wait.run(ctx); err != nil {
		return err
	}
	s.Logf("  velero lists backup %s: the restore can start", name)
	return nil
}

// waitForRecoveryProject waits, bounded, until the operator has created the
// Project of a namespace this restore is about to restore.
//
// THE PROJECT HAS TO BE THERE BEFORE THE RESTORE STARTS, BECAUSE THE RESTORE'S
// FIRST CHANGE IS A PAUSE ON THAT PROJECT. A namespace restore's first write is
// `kubenest.io/reconcile-paused` on the namespace's Project, and on a cluster
// built minutes ago that CR is created by the operator from the control plane's
// desired state: the control plane re-delivers a cluster's projects once its
// new machine has registered (the register stage above), and the operator then
// writes each Project CR. On lab s6 (2026-09-28) the restore reached the
// annotation first and stage 16 failed with `kubectl annotate project s6-data
// ... exit 1: Error from server (NotFound)`, which names the tool rather than
// the object that had not arrived.
//
// THE LOOK IS THE READ THE RESTORE MAKES ITSELF — `get project <namespace> -n
// kubenest-system`, through the same reader pkg/backup uses to judge whether a
// pause was acknowledged (a missing object is reported as no project and no
// error). A wait with its own idea of what "the project is here" means could
// pass and then be told by the pause that it is not, which is the failure this
// wait exists to remove.
//
// NOTHING IS MARKED NOT-RESTORED WHEN THE BOUND RUNS OUT. The namespace is
// untouched and the failure is resumable: the same command re-runs this stage,
// waits for the project again and restores then. Recording it as not restored
// would make the resume leave it held for good.
func (s *Session) waitForRecoveryProject(ctx context.Context, r k3s.Runner, namespace string, deadline time.Duration) error {
	cluster := backup.NewK3sCluster(r)
	wait := recoveryWait{
		Name:     fmt.Sprintf("the project of namespace %s in %s", namespace, backup.ProjectCRNamespace),
		Deadline: deadline,
		Look: func(ctx context.Context) (bool, string, error) {
			hold, err := cluster.ProjectHold(ctx, namespace)
			if err != nil {
				return false, "", err
			}
			if hold == nil {
				return false, "the operator has not created it yet", nil
			}
			return true, "", nil
		},
		Report: func(observation string) {
			s.Logf("  waiting for Project %s/%s on the rebuilt cluster: %s. The control plane re-delivers this cluster's projects once the new machine registers, and its operator then creates the Project CR from the desired state it holds; the restore pauses that CR before it restores the namespace's data", backup.ProjectCRNamespace, namespace, observation)
		},
		Refuse: func(observation string) error {
			return fmt.Errorf("Project %s/%s never arrived on the rebuilt cluster within %s (limits.timeouts.component-ready): %s, so nothing was restored for namespace %s. The control plane re-delivers a cluster's projects once its new machine has registered, and the operator creates each Project CR in %s from the desired state it then holds — until it does, a restore of this namespace has nothing to pause. Run the identical command again once the Project is there: a resume re-runs this stage and does not repeat the stages before it, and namespace %s is restored then", backup.ProjectCRNamespace, namespace, deadline, observation, namespace, backup.ProjectCRNamespace, namespace)
		},
	}
	if err := wait.run(ctx); err != nil {
		return err
	}
	s.Logf("  Project %s/%s is on the rebuilt cluster: its namespace can be restored", backup.ProjectCRNamespace, namespace)
	return nil
}

// recoveryStorageLocationReport describes the BackupStorageLocation kubenest
// manages, for a progress line and for the refusal the deadline produces: its
// name and Velero's phase for it, Velero's own message when it left one, and
// when Velero last validated it. A location that cannot be read at all is
// reported as that, never as a missing one, because the two call for different
// afternoons.
func (s *Session) recoveryStorageLocationReport(ctx context.Context, r k3s.Runner) string {
	state, err := backup.ReadStorageLocation(ctx, r)
	if err != nil {
		return fmt.Sprintf("backupstoragelocation %s could not be read: %v", backup.StorageLocationName, err)
	}
	phase := state.Phase
	if phase == "" {
		// Velero writes no phase until it has run a validation cycle, and
		// "Available" is exactly what that is not.
		phase = "not validated yet"
	}
	report := fmt.Sprintf("backupstoragelocation %s is %s", state.Name, phase)
	if state.Message != "" {
		report += fmt.Sprintf(" (%s)", state.Message)
	}
	if !state.LastValidationTime.IsZero() {
		report += fmt.Sprintf(", last validated %s", state.LastValidationTime.UTC().Format(time.RFC3339))
	}
	return report
}

// recoveryRestoreClock is the clock a bounded wait in this package runs on, in
// the shape pkg/backup's RestoreDeps already carries (Now, Poll, Sleep). It is a
// package variable for the same reason those fields exist: a wait that slept in
// real time could only be tested by spending the ten minutes the bundle allows
// it, and a bound nobody can test is a bound nobody can trust.
type recoveryRestoreClock struct {
	// Now is the current time, UTC.
	Now func() time.Time
	// Poll is how long one wait sleeps between two observations.
	Poll time.Duration
	// Sleep waits for the given duration, or until the context is cancelled.
	Sleep func(ctx context.Context, d time.Duration) error
}

// recoveryRestoreSync is that clock, with the values this CLI waits on in
// production: a five-second poll, the same one pkg/backup's restore uses, and a
// sleep that returns as soon as the operator cancels. A test in this package
// replaces it with a clock it advances itself.
var recoveryRestoreSync = recoveryRestoreClock{
	Now:   func() time.Time { return time.Now().UTC() },
	Poll:  5 * time.Second,
	Sleep: sleepContext,
}

// sleepContext waits for d, or until the context is cancelled. Every wait this
// CLI drives is interruptible: an operator who stops an install must not be
// held for the rest of a ten-minute bound.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// recoveryWait is one bounded wait in this package: the look that answers it,
// the progress line a continuing wait prints, and the refusal the bound
// produces.
//
// IT EXISTS SO EVERY BOUNDED WAIT HERE SHARES ONE CLOCK. The clock is
// injectable only through recoveryRestoreSync, so a wait that grew its own loop
// would be a second copy of the clock logic to keep in step with the tests —
// and a bound nobody can test is a bound nobody can trust. What is being waited
// for stays with the caller: the caller's Look answers that, and its Report and
// Refuse render the same observation the two waits need to say differently.
type recoveryWait struct {
	// Name is the thing being waited for, as a reader of an error or a progress
	// line has to recognise it.
	Name string
	// Deadline bounds the wait. It is always a limit from the bundle manifest,
	// never a number invented here.
	Deadline time.Duration
	// Look asks once whether the thing waited for is here, and describes what
	// this look found. An error is the look itself failing to be carried out,
	// which is an OBSERVATION and not a verdict: an API server restarting must
	// not end a wait the deadline would have let recover. Only the deadline
	// ends it.
	Look func(ctx context.Context) (bool, string, error)
	// Report prints the progress line one continuing look produces. A wait that
	// prints nothing is a CLI that looks hung, and these waits can run for the
	// bundle's whole component-ready deadline.
	Report func(observation string)
	// Refuse builds the failure the deadline produces, from the last look.
	Refuse func(observation string) error
}

// run drives the wait on the recovery clock.
func (w recoveryWait) run(ctx context.Context) error {
	if w.Deadline <= 0 {
		return fmt.Errorf("waiting for %s was given no deadline: every wait in this CLI is bounded by the bundle's limits, and a default here would be an unbounded one", w.Name)
	}
	clock := recoveryRestoreSync
	stop := clock.Now().Add(w.Deadline)
	for {
		here, observation, err := w.Look(ctx)
		if err == nil && here {
			return nil
		}
		if err != nil {
			observation = fmt.Sprintf("%s could not be read: %v", w.Name, err)
		}
		w.Report(observation)
		if !clock.Now().Before(stop) {
			return w.Refuse(observation)
		}
		if err := clock.Sleep(ctx, clock.Poll); err != nil {
			return fmt.Errorf("waiting for %s: %w", w.Name, err)
		}
	}
}

// recoveryRestoreDeps is the dependency set a pkg/backup restore run is driven
// with.
//
// BOTH STAGES THAT DRIVE ONE BUILD IT THE SAME WAY, because they are two steps
// of ONE operation: the restore stage starts it and leaves it awaiting
// activation, and the activate stage finishes it. Same transport, same record,
// same operator name in that record — a second construction that drifted would
// be two executors of one operation.
func recoveryRestoreDeps(server k3s.Runner) backup.RestoreDeps {
	return backup.RestoreDeps{
		Cluster: backup.NewK3sCluster(server),
		Backups: backup.NewVeleroBackups(server),
		Store:   &operation.Store{Runner: server, Operator: recoveryOperator()},
		Runner:  server,
	}
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
	if reason := s.Record.RecoveryRestoreSkipReason; reason != "" {
		// NEVER RESTORE WHAT CANNOT BE SHOWN TO BE COVERED, AND NEVER DEAD-END A
		// RECOVERY: the reason was decided at selection time and is reported by
		// the command; this stage finishes rather than failing.
		s.Logf("  NOT restoring the workloads of namespace(s) %s: %s", coverageOrUnknown(namespaces), reason)
		for _, ns := range coverageOrNames(namespaces) {
			markNotRestored(s, ns, reason)
		}
		if len(namespaces) == 0 && (s.recoveryWorkloadSel != nil || sel.Set.Binding.Kind == recoverykit.KindControlPlane) {
			s.Logf("  the backup names no namespaces, so the report cannot list them; `kubenest backup restore --latest` after choosing a namespace by hand")
		}
		return nil
	}
	if len(namespaces) == 0 {
		return fmt.Errorf("the recovery set's backup %q records no namespace coverage, so there is nothing this stage could restore. A backup whose expected set was never recorded cannot be shown to cover anything — restore the workload by hand with `kubenest backup restore --namespace <ns> --from %s --replace --accept-data-age --confirm`", sel.Backup.Name, sel.Backup.Name)
	}
	// A BACKUP THE BUCKET HOLDS IS NOT YET AN OBJECT ON A CLUSTER THAT WAS JUST
	// BUILT, and asking RunRestore for it before Velero has listed it fails the
	// whole recovery with "is not on this cluster" — which is what stage 16 did
	// on lab s6 (2026-09-28), against a backup that was never the problem.
	// Velero's backup-sync controller copies a bucket's backups onto a cluster
	// on its own schedule (its default backup-sync period is one minute) and
	// only after the storage location has validated; on that cluster the
	// location first reported Available, with its last synced time, at
	// 11:36:58Z, and the backup appeared with it. So wait, bounded, for the
	// backup to be listed.
	//
	// THE DEADLINE IS THE BUNDLE'S, like every other wait in this CLI: a
	// cluster's component becoming ready is what component-ready bounds, and no
	// number is invented here.
	syncDeadline, err := s.Bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return fmt.Errorf("the bundle declares no component-ready deadline, which is the bound this stage gives Velero's backup-sync controller to list the recovery set's backup: %w", err)
	}
	// The read is scoped to one covered namespace because the eligibility
	// reader judges a backup for a namespace; whether Velero has listed the
	// backup is a question about the cluster, and the same object is what the
	// per-namespace judgement below reads.
	if err := s.waitForRecoveryBackup(ctx, server, sel.Backup.Name, namespaces[0], syncDeadline); err != nil {
		return err
	}
	deps := recoveryRestoreDeps(server)
	for _, namespace := range namespaces {
		if containsString(s.Record.RecoveryNamespacesRestored, namespace) {
			s.Logf("  namespace %s was already restored by this recovery: not restoring it again over its own data", namespace)
			continue
		}
		// THE FULL ELIGIBILITY RULE, PER NAMESPACE, NOW THAT THERE IS A CLUSTER
		// TO ASK: completed, coverage known, no missing or failed volume, and
		// the storage location Available. Selection could only judge coverage
		// (the cluster does not exist yet); this is where the rest is knowable.
		// An INELIGIBLE backup is skipped with its reason, never failed: the
		// namespaces stay held and the report says why.
		if facts, err := backup.NewVeleroBackups(server).NamedBackup(ctx, namespace, sel.Backup.Name); err == nil {
			if reason := facts.IneligibleReason(); reason != "" {
				s.Logf("  NOT restoring namespace %s: %s", namespace, reason)
				markNotRestored(s, namespace, reason)
				if err := s.saveRecord(); err != nil {
					return err
				}
				continue
			}
		}
		// THE RESTORE CANNOT START UNTIL THE PROJECT IT PAUSES IS THERE. The
		// first change a namespace restore makes is the reconcile-pause
		// annotation on that namespace's Project, and on the rebuilt cluster the
		// operator creates that CR from the control plane's desired state only
		// after the control plane has re-delivered the cluster's projects to the
		// new machine (which the register stage above triggers). On lab s6
		// (2026-09-28) stage 16 reached the annotation first and failed with
		// `kubectl annotate project s6-data ... exit 1: Error from server
		// (NotFound)`, and the namespace stayed un-restored. The bound is the
		// same bundle deadline the wait for Velero's backup takes, and the
		// failure names the namespace and resumes with the same command.
		if err := s.waitForRecoveryProject(ctx, server, namespace, syncDeadline); err != nil {
			return err
		}
		s.Logf("  restoring namespace %s from %s", namespace, sel.Backup.Name)
		// THE OPERATION ID IS HANDED BACK, NOT READ OUT OF THE OUTPUT. The
		// restore prints `next: kubenest backup restore --activate <id>`, and
		// that line is for the operator: this stage needs the id to run that
		// activation itself once the operator's own release is written, and
		// the record is what a resume reads it from.
		restoredOp := ""
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
			OnOperation:   func(id string) { restoredOp = id },
		}
		if err := backup.RunRestore(ctx, s.Out, nil, opts, deps); err != nil {
			return stages.NewComponentError("recovery-restore", fmt.Errorf("restoring namespace %s from backup %s: %w", namespace, sel.Backup.Name, err))
		}
		if restoredOp != "" {
			if s.Record.RecoveryRestoreOperations == nil {
				s.Record.RecoveryRestoreOperations = map[string]string{}
			}
			s.Record.RecoveryRestoreOperations[namespace] = restoredOp
		}
		s.Record.RecoveryNamespacesRestored = append(s.Record.RecoveryNamespacesRestored, namespace)
		if err := s.saveRecord(); err != nil {
			return err
		}
	}
	return nil
}

// refuseASwitchedSet refuses a resume that would restore from a different
// artifact than the attempts before it.
//
// THE BUCKET MAY HAVE CHANGED BETWEEN ATTEMPTS — a new backup, a new set, a new
// checkpoint — and a recovery that silently followed the newest one would leave
// the namespaces it already restored coming from one artifact and the rest from
// another. The recorded key is the operation's own statement of what it is
// recovering from; a different set is a DIFFERENT recovery, and the operator
// starts one deliberately rather than discovering it half way through.
func (s *Session) refuseASwitchedSet(sel *recovery.Selection) error {
	recorded := s.Record.RecoverySetKey
	if recorded == "" || sel == nil || recorded == sel.SetKey {
		return nil
	}
	return fmt.Errorf("this operation selected %s on an earlier attempt and the bucket now offers %s as the newest eligible set: a resume must not switch artifacts half way through a recovery, because the data already restored came from the first one. Nothing was changed. If the newer set is the one you want, start a NEW recovery from it deliberately", recorded, sel.SetKey)
}

// recoveryActivationValue is the operation id the activation annotation
// carries: the one this attempt took, or — on a resume, where the ownership
// stage did not run and the in-memory id is empty — the one the operation
// recorded when it did.
func (s *Session) recoveryActivationValue() string {
	if s.recoveryOpID != "" {
		return s.recoveryOpID
	}
	return s.Record.RecoveryOperationID
}

// StageRecoveryAPI makes sure this recovery holds a client for the control
// plane it rebuilt. It is AlwaysRun because the stage that builds it holds it
// in memory only: a journal cannot carry a token, so a resume that skipped that
// stage would reach the register stage with no client at all.
const StageRecoveryAPI = "recovery-api"

// stageRecoveryAPI re-establishes the control-plane client on a resume.
func stageRecoveryAPI(ctx context.Context, s *Session) error {
	if s.API != nil {
		return nil
	}
	server, err := s.Server()
	if err != nil {
		return err
	}
	client, err := s.openControlPlaneTunnel(ctx, server)
	if err != nil {
		return err
	}
	s.API = client
	s.Logf("  re-established the connection to the restored control plane this attempt is resuming into")
	return nil
}

// containsString is the membership test the recovery's journalled "already
// done" lists use.
func containsString(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

// stageRecoveryActivate releases the projects recovery mode held, once their
// data is back (PLAN 7.9 step 6).
//
// ACTIVATION IS THE OPERATOR'S POSITIVE DECISION, and in recovery mode nothing
// else releases a project: the operator holds every project until it carries
// the activation annotation, so a pause that is removed is not a release. This
// stage writes that annotation on each restored namespace's project — and
// only after its restore completed, which is what keeps a restored Job, CronJob
// or application container from running against data that is not back.
//
// IT ALSO FINISHES THE RESTORE ITSELF. The operator's annotation releases the
// project from RECOVERY MODE's hold; the namespace restore's own pause is a
// SECOND hold, and only `backup restore --activate <that operation>` lifts it,
// puts the CronJobs back to the suspend value the backup held and scales the
// workloads back up. Writing the recovery's annotation alone left a namespace
// restored, paused and with its scheduled work off (lab s6, 2026-09-28), which
// is the defect this stage now closes.
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
	// THE SAME DEPS THE RESTORE STAGE BUILT, because this stage finishes the
	// operation that stage started.
	deps := recoveryRestoreDeps(server)
	deadline, err := s.Bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return fmt.Errorf("the bundle declares no component-ready timeout, so waiting for a project to appear has no deadline and this stage will not guess one: %w", err)
	}
	for _, namespace := range s.workloadSelection().Backup.Coverage {
		if containsString(s.Record.RecoveryNamespacesActivated, namespace) {
			s.Logf("  %s was already activated by this recovery", namespace)
			continue
		}
		if containsString(s.Record.RecoveryNamespacesNotRestored, namespace) {
			s.Logf("  %s stays HELD: its data was not restored (%s), and activating it would start its workloads against whatever is in the namespace rather than against the data they expect", namespace, s.Record.RecoveryRestoreSkipReason)
			continue
		}
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
		s.Logf("  activated %s: %s=%s on project %s/%s", ns, backup.ActivateAnnotationKey, s.recoveryActivationValue(), backup.ProjectCRNamespace, ns)
		// THE OPERATOR'S RELEASE IS ONLY HALF OF IT. The namespace's data was
		// put back by ITS OWN restore operation, which stopped at `restored —
		// awaiting activation` with the project paused and every CronJob
		// suspended; that operation has to be activated too, or the project
		// keeps the restore's pause and the scheduled work never runs. On lab
		// s6 (2026-09-28) the project carried both the recovery's activation
		// and the namespace restore's pause, and `kn-recovery-sentinel` stayed
		// suspended for ever.
		if err := s.activateNamespaceRestore(ctx, deps, ns); err != nil {
			return err
		}
		s.Record.RecoveryNamespacesActivated = append(s.Record.RecoveryNamespacesActivated, ns)
		if err := s.saveRecord(); err != nil {
			return err
		}
	}
	return nil
}

// activateNamespaceRestore finishes the pkg/backup operation that restored one
// namespace: it lifts that operation's pause and puts the workloads and the
// CronJobs back to the state the BACKUP held.
//
// IT RUNS AFTER THE OPERATOR'S OWN RELEASE, because the two holds are
// independent and the recovery's own annotation is what this stage exists to
// write; a failure between them leaves the namespace held by the restore's
// pause, which is the state the hardware was in and a state a resume repairs.
//
// WHICH OPERATION IT IS COMES FROM THE RECORD. The restore stage was handed the
// id by pkg/backup and wrote it down per namespace, and a resume that re-enters
// this stage without any restore in memory reads it back from there. A journal
// written before this CLI recorded it names the restored namespaces but not
// their operations, so the pause annotation the restore itself wrote on the
// project is the fallback: it is the same fact, from the same actor, and it is
// the only other place a recovery can read it.
func (s *Session) activateNamespaceRestore(ctx context.Context, deps backup.RestoreDeps, ns string) error {
	restoreOp := s.Record.RecoveryRestoreOperations[ns]
	if restoreOp == "" {
		hold, err := deps.Cluster.ProjectHold(ctx, ns)
		if err != nil {
			return err
		}
		if hold != nil {
			restoreOp = hold.PausedBy
		}
	}
	if restoreOp == "" {
		// NO RESTORE'S PAUSE IS ON THIS PROJECT, so the thing activation would
		// lift is already gone: the operation was activated by an earlier
		// attempt (a crash between this stage's two steps is exactly that) and
		// only the journal write was lost. There is nothing to activate and
		// nothing to refuse.
		s.Logf("  %s carries no pause from a namespace restore, so that restore was activated already", ns)
		return nil
	}
	done, err := restoreActivationDone(ctx, deps.Store, restoreOp)
	if err != nil {
		return err
	}
	if done {
		s.Logf("  the namespace restore of %s (operation %s) is complete: activation already ran, so it is not run again", ns, restoreOp)
		return nil
	}
	opts := backup.RestoreOptions{
		Cluster:   s.recoveryName(),
		Namespace: ns,
		Activate:  restoreOp,
		Confirm:   true,
		// THE CONTROL PLANE'S DESIRED STATE WINS where the restored
		// configuration differs from it. The rebuilt cluster's desired state
		// was re-delivered by the control plane onto a machine that had no
		// configuration at all, so it is the newer statement of what the
		// cluster should hold; the restored copy is what the dead host last
		// saw. Activation's other answer, --keep-restored, would leave two
		// statements of intent and let reconciliation decide (S4's own gate
		// activates with --keep-desired for the same reason).
		KeepDesired: true,
		Bundle:      s.Bundle,
	}
	if err := backup.RunRestore(ctx, s.Out, nil, opts, deps); err != nil {
		return stages.NewComponentError("recovery-restore", fmt.Errorf("activating the namespace restore of %s (operation %s): %w. Its data is back and the project is released by this recovery; run the same install again to resume here", ns, restoreOp, err))
	}
	s.Logf("  activated the namespace restore of %s: operation %s is closed, its pause is lifted and its scheduled work is back to the state the backup held", ns, restoreOp)
	return nil
}

// restoreActivationDone reports whether the restore operation this recovery is
// about to activate has already been activated.
//
// THE ONE CASE A RESUME HAS TO RECOGNISE is an operation that is already
// finished: activation closes its record, so a run that crashed after the
// restore's activation and before the journal write that follows it finds a
// TERMINAL record, and `--activate` refuses a terminal operation outright
// ("there is nothing to do for it"). That refusal is right for an operator
// asking again and wrong here, where the record is the proof the step is done.
func restoreActivationDone(ctx context.Context, store *operation.Store, opID string) (bool, error) {
	stored, err := store.Find(ctx, opID)
	if err != nil {
		return false, fmt.Errorf("reading the record of the namespace restore %s, which is what says whether it still needs activating: %w", opID, err)
	}
	if !stored.Record.Terminal {
		return false, nil
	}
	// Complete appends the outstanding writes to the result ("succeeded;
	// pending: …"), so a successful operation is recognised by its first word
	// rather than by the whole string.
	if strings.HasPrefix(stored.Record.Result, string(operation.ResultSucceeded)) {
		return true, nil
	}
	return false, fmt.Errorf("the namespace restore %s is terminal (%s), so it never reached its activation and this recovery must not close it over: %s. Its data is back and the namespace stays held; inspect the operation before activating it by hand", opID, stored.Record.Result, backupActivationAdvice(opID))
}

// backupActivationAdvice names the command that finishes a namespace restore by
// hand, which is the same step this stage just could not take.
func backupActivationAdvice(opID string) string {
	return fmt.Sprintf("`kubenest backup restore --activate %s`", opID)
}

// markNotRestored records a namespace whose data was not restored, so the
// report names it and activation leaves it held.
func markNotRestored(s *Session, namespace, reason string) {
	if !containsString(s.Record.RecoveryNamespacesNotRestored, namespace) {
		s.Record.RecoveryNamespacesNotRestored = append(s.Record.RecoveryNamespacesNotRestored, namespace)
	}
	if s.Record.RecoveryRestoreSkipReason == "" {
		s.Record.RecoveryRestoreSkipReason = reason
	}
}

// coverageOrUnknown names the namespaces a backup is known to cover, or says
// that nothing knows.
func coverageOrUnknown(namespaces []string) string {
	if len(namespaces) == 0 {
		return "(the backup records no coverage, so no namespace can be named)"
	}
	return strings.Join(namespaces, ", ")
}

// coverageOrNames is the list to record, which is empty when nothing is known.
func coverageOrNames(namespaces []string) []string { return namespaces }
