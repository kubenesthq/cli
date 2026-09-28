package install

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/component/agent"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/recovery"
	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/s3"
	"kubenest.io/cli/pkg/sshx"
)

// The recovery install's unit tests. They are in package install (not
// install_test) because a recovery's session state — the set it selected, the
// bucket it read, the copy it claims through — is what several of these
// assertions are about, and exporting those fields for a test would be a worse
// trade than reading them where they live.

// recoveryTestScope is the bucket prefix a test's artifacts live under.
const recoveryTestScope = "clusters/s6"

// fakeBucket is the slice of an S3-compatible store the recovery reads, with
// the writes counted so "nothing was written" is an assertion rather than a
// hope.
type fakeBucket struct {
	objects map[string][]byte
	puts    int
}

func (f *fakeBucket) Get(_ context.Context, key string) ([]byte, error) {
	if body, ok := f.objects[key]; ok {
		return body, nil
	}
	return nil, s3.ErrNotFound
}

func (f *fakeBucket) List(_ context.Context, prefix string) ([]string, bool, error) {
	var keys []string
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, false, nil
}

// fakeWriter is the off-cluster object writer a claim writes through.
type fakeWriter struct {
	objects map[string][]byte
	puts    int
	present map[string]bool
}

func (f *fakeWriter) Put(_ context.Context, key string, body []byte) error {
	f.puts++
	f.objects[key] = body
	return nil
}

func (f *fakeWriter) PutIfAbsent(_ context.Context, key string, body []byte) (bool, error) {
	if f.present == nil {
		f.present = map[string]bool{}
	}
	if f.present[key] {
		return false, nil
	}
	f.present[key] = true
	f.objects[key] = body
	f.puts++
	return true, nil
}

// recordingRunner answers each command from a table and records every command
// it was given, so a test can assert both what was run and what was NOT.
type recordingRunner struct {
	respond func(command string) (sshx.Result, error)
	ran     []string
	inputs  [][]byte
}

func (r *recordingRunner) Run(_ context.Context, command string) (sshx.Result, error) {
	r.ran = append(r.ran, command)
	if r.respond == nil {
		return sshx.Result{}, nil
	}
	return r.respond(command)
}

func (r *recordingRunner) RunInput(_ context.Context, command string, in io.Reader) (sshx.Result, error) {
	r.ran = append(r.ran, command)
	body, _ := io.ReadAll(in)
	r.inputs = append(r.inputs, body)
	if r.respond == nil {
		return sshx.Result{}, nil
	}
	return r.respond(command)
}

// ranAny reports whether any command this run issued contains one of the
// fragments — used to assert that NOTHING destructive was reached.
func (r *recordingRunner) ranAny(fragments ...string) bool {
	for _, command := range r.ran {
		for _, fragment := range fragments {
			if strings.Contains(command, fragment) {
				return true
			}
		}
	}
	return false
}

func (r *recordingRunner) ranAll() string { return strings.Join(r.ran, "\n") }

// recoveryFixture is one cluster's recovery artifacts, sealed to a throw-away
// fleet key.
type recoveryFixture struct {
	scope  string
	fleet  *recoverykit.FleetKey
	bucket *fakeBucket
	bind   recoverykit.Binding
	art    string
	// versions is what the set declares, including the storage requirement the
	// capacity check reads.
	versions map[string]string
	// secrets is the kit's key material, so a test can open it.
	secrets map[string]string
}

func newRecoveryFixture(t *testing.T, clusterID, orgID, instanceID string, versions map[string]string) *recoveryFixture {
	t.Helper()
	fleet, err := recoverykit.GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}
	if versions == nil {
		versions = map[string]string{"bundle": "1.1"}
	}
	f := &recoveryFixture{
		scope:  recoveryTestScope,
		fleet:  fleet,
		bucket: &fakeBucket{objects: map[string][]byte{}},
		bind: recoverykit.Binding{
			Kind:           recoverykit.KindCluster,
			InstanceID:     instanceID,
			OrganisationID: orgID,
			ClusterID:      clusterID,
		},
		art:      "20260926T101010Z-aaaaaaaa",
		versions: versions,
		secrets: map[string]string{
			recoverykit.KeyVeleroRepoPassword: "repo-password-from-the-kit",
			recoverykit.KeyK3sJoinToken:       "join-token-from-the-kit",
		},
	}
	f.publish(t, f.bind, f.art, f.secrets, f.versions, true, recoverykit.Backup{
		Name:        "daily-20260926",
		CompletedAt: time.Date(2026, 9, 26, 2, 0, 0, 0, time.UTC),
		Coverage:    []string{"payments", "storefront"},
		Status:      "Completed",
	})
	return f
}

// publish writes one kit and one set, sealed exactly as the install writes them.
func (f *recoveryFixture) publish(t *testing.T, bind recoverykit.Binding, artifact string, secrets, versions map[string]string, complete bool, backups ...recoverykit.Backup) {
	t.Helper()
	now := time.Date(2026, 9, 26, 10, 10, 10, 0, time.UTC)
	kit, err := recoverykit.New(bind, artifact, recoverykit.Location{
		Endpoint: "minio.example.test", Bucket: "kubenest-kit", Region: "main", Prefix: f.scope,
	}, "velero-default-kopia", secrets, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := kit.SealTo(f.fleet.Recipient()); err != nil {
		t.Fatal(err)
	}
	doc, err := kit.Document()
	if err != nil {
		t.Fatal(err)
	}
	set, err := recoverykit.NewSet(kit, versions, recoverykit.Digest(doc), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range backups {
		set, err = set.WithBackup(b)
		if err != nil {
			t.Fatal(err)
		}
	}
	set.Complete = complete
	if !complete {
		delete(set.Checksums, "kit")
	}
	setDoc, err := set.Document()
	if err != nil {
		t.Fatal(err)
	}
	f.bucket.objects[recoverykit.KitKey(f.scope, bind.ClusterID, bind.Kind, artifact)] = doc
	f.bucket.objects[recoverykit.SetKey(f.scope, bind.ClusterID, bind.Kind, artifact)] = setDoc
}

// request is the selection this fixture's own cluster asks for.
func (f *recoveryFixture) request() recovery.Request {
	return recovery.Request{Kind: recoverykit.KindCluster, ClusterID: f.bind.ClusterID, Expected: f.bind}
}

// recoverySession builds a session in the recovery shape with a server node the
// test controls and no control-plane client: the stages under test decide
// before a client matters, and the ones that need one are refused by name.
func recoverySession(t *testing.T, opts Options) (*Session, *recordingRunner) {
	t.Helper()
	m, err := manifest.Parse([]byte("bundle: \"1.1\"\nlimits:\n  timeouts:\n    install-total: 30m\n    component-ready: 5m\n"))
	if err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(filepath.Join(t.TempDir(), "journal.json"), opts.Identity())
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	return &Session{
		ID:     "run-1",
		Opts:   opts,
		Bundle: m,
		Jnl:    j,
		Emit:   NopEmitter{},
		Out:    io.Discard,
		Nodes:  []Node{{Address: "10.0.9.9", Role: RoleServer, Runner: runner}},
	}, runner
}

// recoveryOptions is a workload-cluster recovery request for one fixture.
func recoveryOptions(f *recoveryFixture, fleetKey string) Options {
	return Options{
		Bundle:  "1.1",
		Servers: []string{"10.0.9.9"},
		HATier:  "single-server",
		Recovery: &RecoveryOptions{
			ClusterID:     f.bind.ClusterID,
			RestoreFrom:   "latest",
			Kit:           "s3",
			FleetKey:      fleetKey,
			OldHostFenced: true,
			Kind:          recoverykit.KindCluster,
		},
	}
}

// TestRecoverySetIsSelectedByImmutableIDNotDisplayName is the planted negative
// the bead names first: adoption is by the cluster's IMMUTABLE id, and a set
// that belongs to another cluster is refused by name rather than used.
//
// It also asserts the display-name path is dead rather than merely unused: a
// request that names a display name is refused by the selection itself, and a
// set sitting under the right directory but bound to another cluster is
// refused with what it actually belongs to.
func TestRecoverySetIsSelectedByImmutableIDNotDisplayName(t *testing.T) {
	const clusterA = "01a02362-f8a3-7dd6-aa07-2f10ed7a5c17"
	const clusterB = "01a02362-f8a3-7dd6-aa07-2f10ed7a5c18"
	mine := newRecoveryFixture(t, clusterA, "org-1", "inst-1", nil)
	// The other cluster's set, under its own directory. Selecting by A's id
	// must never pick it up.
	mine.publish(t, recoverykit.Binding{Kind: recoverykit.KindCluster, InstanceID: "inst-1", OrganisationID: "org-1", ClusterID: clusterB},
		"20260926T111111Z-bbbbbbbb", mine.secrets, nil, true, recoverykit.Backup{
			Name: "daily-b", CompletedAt: time.Date(2026, 9, 26, 3, 0, 0, 0, time.UTC), Status: "Completed",
		})

	sel, err := recovery.Select(context.Background(), mine.bucket, mine.scope, mine.request())
	if err != nil {
		t.Fatalf("selecting cluster %s by its immutable id: %v", clusterA, err)
	}
	if sel.Set.Binding.ClusterID != clusterA || sel.Set.ArtifactID != mine.art {
		t.Fatalf("selected %s for cluster %s and wanted artifact %s: a recovery of one cluster must not be able to select another cluster's set, however similar their names are",
			sel.Set.ArtifactID, sel.Set.Binding.ClusterID, mine.art)
	}

	// A display name is not an id. The request that would come from
	// `--restore-from latest --cluster prod-1` has no cluster id to compare
	// against the set's binding, and that is refused rather than treated as a
	// cluster that happens to be called prod-1.
	_, err = recovery.Select(context.Background(), mine.bucket, mine.scope, recovery.Request{
		Kind:      recoverykit.KindCluster,
		ClusterID: "prod-1",
		Expected: recoverykit.Binding{
			Kind: recoverykit.KindCluster, InstanceID: "inst-1", OrganisationID: "org-1", ClusterID: clusterA,
		},
	})
	if err == nil {
		t.Fatal("selecting a cluster by the display name \"prod-1\" was accepted: a display name never authorises adoption")
	}
	if !strings.Contains(err.Error(), "display name") {
		t.Fatalf("the refusal of a display-name selection does not name the rule it is enforcing: %v", err)
	}

	// And a set COPIED under this cluster's directory but bound to another
	// cluster is refused with what it actually belongs to, rather than used
	// because it is where this cluster looks.
	foreign := newRecoveryFixture(t, clusterB, "org-1", "inst-1", nil)
	mine.bucket.objects[recoverykit.SetKey(mine.scope, clusterA, recoverykit.KindCluster, "20260926T121212Z-cccccccc")] =
		foreign.bucket.objects[recoverykit.SetKey(mine.scope, clusterB, recoverykit.KindCluster, foreign.art)]
	_, err = recovery.Select(context.Background(), mine.bucket, mine.scope, mine.request())
	if err == nil {
		t.Fatal("a set bound to another cluster was accepted at this cluster's own path")
	}
	if !strings.Contains(err.Error(), clusterB) || !strings.Contains(err.Error(), "org-1") || !strings.Contains(err.Error(), "inst-1") {
		t.Fatalf("the refusal does not name the instance, organisation and cluster the set actually belongs to: %v", err)
	}
}

// TestForeignOrIncompleteSetIsRefusedBeforeAnyDestructiveStep asserts the
// ordering the plan exists to guarantee: every refusal happens before anything
// is written anywhere.
func TestForeignOrIncompleteSetIsRefusedBeforeAnyDestructiveStep(t *testing.T) {
	const clusterA = "01a02362-f8a3-7dd6-aa07-2f10ed7a5c19"
	f := newRecoveryFixture(t, clusterA, "org-1", "inst-1", nil)

	// An INCOMPLETE set is never a candidate, even when it names a completed
	// backup: the upload that produced it did not finish, so its kit has no
	// digest to compare against and nothing here can say the dump in the bucket
	// is the one the set was written for.
	f.publish(t, f.bind, "20260926T131313Z-dddddddd", f.secrets, nil, false, recoverykit.Backup{
		Name: "daily-partial", CompletedAt: time.Date(2026, 9, 26, 4, 0, 0, 0, time.UTC),
		Coverage: []string{"payments"}, Status: "Completed",
	})
	if _, err := recovery.Select(context.Background(), f.bucket, f.scope, recovery.Request{
		Kind: recoverykit.KindCluster, ClusterID: clusterA,
		Expected: f.bind, ArtifactID: "20260926T131313Z-dddddddd",
	}); err == nil {
		t.Fatal("an incomplete recovery set was selected: a partial upload is never something to recover from")
	}

	// A FOREIGN set is refused outright, naming what it belongs to.
	other := newRecoveryFixture(t, "01a02362-f8a3-7dd6-aa07-2f10ed7a5c20", "org-9", "inst-9", nil)
	_, err := recovery.Select(context.Background(), other.bucket, other.scope, recovery.Request{
		Kind: recoverykit.KindCluster, ClusterID: "01a02362-f8a3-7dd6-aa07-2f10ed7a5c20",
		// This machine believes it is instance inst-1, organisation org-1: the
		// set it finds says otherwise, and that is what must be refused.
		Expected: recoverykit.Binding{
			Kind: recoverykit.KindCluster, InstanceID: "inst-1", OrganisationID: "org-1",
			ClusterID: "01a02362-f8a3-7dd6-aa07-2f10ed7a5c20",
		},
	})
	if err == nil {
		t.Fatal("a set belonging to another instance and organisation was accepted")
	}
	for _, want := range []string{"inst-9", "org-9"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %s, so the operator cannot see which prefix they are reading: %v", want, err)
		}
	}

	// The pre-flight refuses the same set, and by then the assertion is that
	// NOTHING has run: no command on the host, no object in the bucket, no
	// operation owned.
	s, runner := recoverySession(t, recoveryOptions(f, f.fleet.SecretKeyString()))
	s.recoveryStore = f.bucket
	s.recoveryTargetValue = testTarget(t)
	s.recoverySel = nil
	if err := stageRecoveryPreflight(context.Background(), s); err == nil {
		t.Fatal("the pre-flight ran with no recovery set selected")
	}
	if len(runner.ran) != 0 {
		t.Fatalf("a refused pre-flight issued %d command(s) on the replacement host: %s", len(runner.ran), runner.ranAll())
	}
	if f.bucket.puts != 0 {
		t.Fatalf("a refused pre-flight wrote %d object(s) to the bucket", f.bucket.puts)
	}
	if s.recoveryOpID != "" {
		t.Fatalf("a refused pre-flight took recovery ownership of operation %s", s.recoveryOpID)
	}

	// And the plan puts ownership AFTER the pre-flight, so a refusal can never
	// be reached with the bucket already claimed.
	plan := Plan(s)
	preflight := stageIndex(t, plan, StageRecoveryPreflight)
	ownership := stageIndex(t, plan, StageRecoveryOwnership)
	if preflight > ownership {
		t.Fatalf("the plan claims recovery ownership at %d and runs the pre-flight at %d: ownership must come after every refusal", ownership, preflight)
	}
	selectStage := stageIndex(t, plan, StageRecoverySelect)
	if selectStage > preflight {
		t.Fatalf("the plan selects the recovery set at %d and checks it at %d", selectStage, preflight)
	}
}

// stageIndex finds a stage's position in a plan, failing when it is absent.
func stageIndex(t *testing.T, plan []Stage, name string) int {
	t.Helper()
	for i, stage := range plan {
		if stage.Name == name {
			return i
		}
	}
	t.Fatalf("the recovery plan has no %q stage", name)
	return -1
}

// TestReplacementHostTooSmallIsRefusedAtPreflight is the planted negative about
// capacity: a host that cannot hold the volumes about to be restored is refused
// before anything is written, naming the volume group and the shortfall.
func TestReplacementHostTooSmallIsRefusedAtPreflight(t *testing.T) {
	const clusterA = "01a02362-f8a3-7dd6-aa07-2f10ed7a5c21"
	const fiftyGiB = 53687091200
	const fiveGiB = 5368709120
	f := newRecoveryFixture(t, clusterA, "org-1", "inst-1", map[string]string{
		"bundle":                       "1.1",
		recovery.StorageRequirementKey: fmt.Sprintf("%d", fiftyGiB),
	})

	// A five-gigabyte volume group cannot hold what fifty gigabytes of volumes
	// need.
	s, runner := recoverySession(t, recoveryOptions(f, f.fleet.SecretKeyString()))
	s.recoveryStore = f.bucket
	s.recoveryTargetValue = testTarget(t)
	sel, err := recovery.Select(context.Background(), f.bucket, f.scope, f.request())
	if err != nil {
		t.Fatal(err)
	}
	s.recoverySel = sel
	runner.respond = volumeGroupAnswer(fiveGiB)
	err = stageRecoveryPreflight(context.Background(), s)
	if err == nil {
		t.Fatal("a replacement host too small for the restored volumes passed the pre-flight")
	}
	for _, want := range []string{"kubenest-vg", fmt.Sprintf("%d", fiftyGiB-fiveGiB), "too small"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q, so the operator cannot tell what to fix: %v", want, err)
		}
	}
	if s.recoveryOpID != "" {
		t.Fatal("the pre-flight took ownership before it refused on capacity")
	}

	// The same host, big enough, passes: the refusal is about the shortfall and
	// not about refusing recoveries.
	big, bigRunner := recoverySession(t, recoveryOptions(f, f.fleet.SecretKeyString()))
	big.recoveryStore = f.bucket
	big.recoveryTargetValue = testTarget(t)
	big.recoverySel = sel
	bigRunner.respond = volumeGroupAnswer(2 * fiftyGiB)
	if err := stageRecoveryPreflight(context.Background(), big); err != nil {
		t.Fatalf("a host with twice the required capacity was refused: %v", err)
	}
	if len(bigRunner.ran) == 0 {
		t.Fatal("the pre-flight never asked the host about its volume group, so the check did not run")
	}
}

// volumeGroupAnswer answers the volume-group query with one size and nothing
// else, which is what a host that has the group looks like.
func volumeGroupAnswer(bytes int64) func(string) (sshx.Result, error) {
	return func(command string) (sshx.Result, error) {
		if strings.Contains(command, "vgs ") {
			return sshx.Result{Stdout: fmt.Sprintf("  %d\n", bytes)}, nil
		}
		return sshx.Result{}, nil
	}
}

// TestRecoveryModeHoldsEveryProjectBeforeTheOperatorStarts asserts the two
// halves of step 3: the operator is installed with the hold ON, and the plan
// reaches the operator only after the incarnation is registered, so nothing can
// reconcile against data that is not restored yet.
func TestRecoveryModeHoldsEveryProjectBeforeTheOperatorStarts(t *testing.T) {
	const clusterA = "01a02362-f8a3-7dd6-aa07-2f10ed7a5c22"
	f := newRecoveryFixture(t, clusterA, "org-1", "inst-1", nil)
	s, _ := recoverySession(t, recoveryOptions(f, f.fleet.SecretKeyString()))
	plan := Plan(s)

	// The operator's own values carry the hold, and the mode is what turns it
	// on: an operator without it starts reconciling, which is the state
	// recovery mode exists to prevent.
	creds := testAgentCredentials()
	held, err := agent.Values(creds, agent.ValuesOptions{RecoveryMode: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(held, "recoveryMode: true") {
		t.Fatalf("the operator installed by a recovery does not carry the hold, so every project would reconcile against data that is not restored yet:\n%s", held)
	}
	plain, err := agent.Values(creds, agent.ValuesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "recoveryMode: true") {
		t.Fatal("an ordinary install renders recovery mode too, which would hold every project of every cluster")
	}

	// The order: the incarnation is registered, and the operator that serves it
	// is installed held. The restore and the activation come after both, so
	// nothing a restored workload carries can run before its data is back.
	register := stageIndex(t, plan, StageRegister)
	operator := stageIndex(t, plan, StageAgent)
	restore := stageIndex(t, plan, StageRecoveryRestore)
	activate := stageIndex(t, plan, StageRecoveryActivate)
	if register > operator {
		t.Fatalf("the plan installs the operator at %d and registers the incarnation at %d: the hold must be in place when the operator first starts", operator, register)
	}
	if operator > restore || restore > activate {
		t.Fatalf("the recovery plan runs operator=%d restore=%d activate=%d: activation must be last, or a restored Job runs before its data is back", operator, restore, activate)
	}
	// The kit is never rewritten: the set this recovery selected must keep
	// meaning what it means.
	for _, stage := range plan {
		if stage.Name == StageRecoveryKit {
			t.Fatal("the recovery plan writes a recovery kit, which would create a new artifact and change what the set this recovery selected means")
		}
	}
}

// TestExistingRepositoryIsOpenedNotReinitialised is the check that is not
// vacuous: the repository identity must be unchanged, the kit's password must
// be the one on the cluster, and nothing may create, delete or re-initialise a
// repository.
func TestExistingRepositoryIsOpenedNotReinitialised(t *testing.T) {
	const password = "repo-password-from-the-kit"
	const repoID = "velero-default-kopia"

	// The cluster already holds the kit's password and the repository object,
	// which is the state after an interrupted recovery or a resume.
	runner := &recordingRunner{respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.Contains(command, "get secret velero-repo-credentials"):
			return sshx.Result{Stdout: secretJSON(t, password)}, nil
		case strings.Contains(command, "get backuprepository"):
			return sshx.Result{Stdout: `{"items":[{"metadata":{"name":"` + repoID + `"}}]}`}, nil
		}
		return sshx.Result{}, nil
	}}
	state, err := recovery.OpenRepository(context.Background(), runner, recovery.Repository{ID: repoID, Password: password})
	if err != nil {
		t.Fatalf("opening the repository: %v", err)
	}
	if state.RepositoryID != repoID {
		t.Fatalf("the repository identity changed: %q, wanted %q", state.RepositoryID, repoID)
	}
	if !state.PasswordMatches || !state.SecretExisted {
		t.Fatalf("the repository was not opened with the kit's password: %+v", state)
	}
	if runner.ranAny(" create ", " delete ", " apply ", "repo init") {
		t.Fatalf("opening an existing repository ran a write: %s", runner.ranAll())
	}

	// A cluster pointing at a repository whose password is NOT the kit's is a
	// WRONG KIT, refused rather than retried: kopia's password is a repository
	// key, and retrying is how a repository gets re-encrypted over.
	wrong := &recordingRunner{respond: func(command string) (sshx.Result, error) {
		if strings.Contains(command, "get secret velero-repo-credentials") {
			return sshx.Result{Stdout: secretJSON(t, "some-other-password")}, nil
		}
		return sshx.Result{}, nil
	}}
	if _, err := recovery.OpenRepository(context.Background(), wrong, recovery.Repository{ID: repoID, Password: password}); err == nil {
		t.Fatal("a repository whose password is not the kit's was opened anyway")
	}
	if wrong.ranAny(" create ", " delete ", " apply ") {
		t.Fatalf("the refusal wrote something: %s", wrong.ranAll())
	}

	// A fresh host has no Secret at all. It is created FROM THE KIT's password
	// — never generated — before Velero starts, because Velero's own setup
	// writes a vendored default into an absent Secret and the repository is
	// then opened under that.
	fresh := &recordingRunner{respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.Contains(command, "get secret velero-repo-credentials"):
			return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): secrets "velero-repo-credentials" not found`}, nil
		case strings.Contains(command, "get backuprepository"):
			return sshx.Result{ExitCode: 1, Stderr: "the server doesn't have a resource type"}, nil
		}
		return sshx.Result{}, nil
	}}
	freshState, err := recovery.OpenRepository(context.Background(), fresh, recovery.Repository{ID: repoID, Password: password})
	if err != nil {
		t.Fatalf("opening the repository on a fresh host: %v", err)
	}
	if freshState.SecretExisted || !freshState.PasswordMatches {
		t.Fatalf("a fresh host's secret was not created from the kit: %+v", freshState)
	}
	if !fresh.ranAny("kubectl create -f -") {
		t.Fatalf("the repository password was not created with kubectl create, which is what stops two writers: %s", fresh.ranAll())
	}
	carried := false
	for _, in := range fresh.inputs {
		if strings.Contains(string(in), password) {
			carried = true
		}
	}
	if !carried {
		t.Fatal("the secret that was written does not carry the kit's password, so the repository would be opened under a password the kit does not hold")
	}
	if fresh.ranAny("repo init", "backuprepository --all", "delete ") {
		t.Fatalf("a fresh host's recovery initialised or deleted a repository: %s", fresh.ranAll())
	}
}

// testAgentCredentials is one minted set of credentials, in the shape the mint
// returns: the operator values renderer refuses a half-populated one, so the
// test builds a whole one.
func testAgentCredentials() *api.AgentCredentials {
	return &api.AgentCredentials{
		ClusterID: "01a02362-f8a3-7dd6-aa07-2f10ed7a5c22",
		AgentJWT: api.AgentJWT{
			Token:        api.NewSecret("agent-token"),
			ExpiresAt:    time.Date(2027, 9, 26, 0, 0, 0, 0, time.UTC),
			HubURL:       "wss://hub.example.test/ws/operator",
			TokenVersion: 2,
		},
	}
}

// secretJSON renders the repository-password Secret the way kubectl returns it.
func secretJSON(t *testing.T, password string) string {
	t.Helper()
	doc := map[string]any{
		"data": map[string]string{
			"repository-password": base64.StdEncoding.EncodeToString([]byte(password)),
		},
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// testTarget is a parsed backup target for the recovery stages that need the
// bucket co-ordinates. Credentials come from the environment, as every backup
// path in this CLI reads them.
func testTarget(t *testing.T) backup.Target {
	t.Helper()
	t.Setenv("KUBENEST_BACKUP_ACCESS_KEY_ID", "test-key")
	t.Setenv("KUBENEST_BACKUP_SECRET_ACCESS_KEY", "test-secret")
	target, err := parseBackupTarget("s3://kubenest-kit/" + recoveryTestScope + "?endpoint=minio.example.test&region=main")
	if err != nil {
		t.Fatal(err)
	}
	return target
}

// ---------------------------------------------------------------------------
// T4.8: the all-in-one host recovery. The same file as T4.9's tests because the
// two procedures share the selection, the ownership and the capacity checks;
// what is here is what only the control-plane shape has.
// ---------------------------------------------------------------------------

// TestRestoreRefusesANewerControlPlaneVersion is the first half of the version
// refusal the bead plants: a checkpoint from a NEWER control plane carries a
// schema this build does not serve, and installing this build in front of it is
// serving older code over a newer database.
func TestRestoreRefusesANewerControlPlaneVersion(t *testing.T) {
	target := recovery.ControlPlaneTarget{Version: "1.2", PostgresMajor: 17, PostgresImage: "bitnami/postgresql@sha256:aaaa"}

	// A checkpoint from the version being installed, and one from older, are
	// both servable: this restore is same-version-or-newer-code.
	for _, version := range []string{"1.2", "1.1", "1.0.4"} {
		if err := recovery.CheckCheckpoint(
			recovery.NewControlPlaneCheckpoint(version, 17, "bitnami/postgresql@sha256:aaaa", true), target,
		); err != nil {
			t.Fatalf("a checkpoint from control plane %s was refused by a %s CLI: %v", version, target.Version, err)
		}
	}

	err := recovery.CheckCheckpoint(
		recovery.NewControlPlaneCheckpoint("1.3", 17, "bitnami/postgresql@sha256:aaaa", true), target,
	)
	if err == nil {
		t.Fatal("a checkpoint from a NEWER control plane was accepted by an older CLI: the schema inside it is one this build does not serve")
	}
	for _, want := range []string{"1.3", "1.2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q, so the operator cannot tell which side is too old: %v", want, err)
		}
	}
	if !strings.Contains(strings.ToLower(err.Error()), "newer") {
		t.Fatalf("the refusal does not say which way the mismatch goes: %v", err)
	}

	// A checkpoint whose upload never completed is never offered as latest,
	// whatever version it came from.
	if err := recovery.CheckCheckpoint(
		recovery.NewControlPlaneCheckpoint("1.2", 17, "bitnami/postgresql@sha256:aaaa", false), target,
	); err == nil {
		t.Fatal("a checkpoint whose upload did not complete was offered as latest")
	}
	if err := recovery.CheckCheckpoint(nil, target); err == nil {
		t.Fatal("a recovery with no eligible checkpoint at all was accepted")
	}
}

// TestRestoreRefusesADifferentPostgresMajor is the second half: this release
// restores same-major only, because a cross-major load is a migration with its
// own tested procedure rather than a recovery.
func TestRestoreRefusesADifferentPostgresMajor(t *testing.T) {
	target := recovery.ControlPlaneTarget{
		Version: "1.2", PostgresMajor: 17, PostgresImage: "bitnami/postgresql@sha256:aaaa",
	}
	// The same major, however the image is spelled, is what this path supports.
	if err := recovery.CheckCheckpoint(
		recovery.NewControlPlaneCheckpoint("1.2", 17, "bitnami/postgresql@sha256:bbbb", true), target,
	); err != nil {
		t.Fatalf("a checkpoint from the chart's own Postgres major was refused: %v", err)
	}

	err := recovery.CheckCheckpoint(
		recovery.NewControlPlaneCheckpoint("1.2", 16, "bitnami/postgresql@sha256:cccc", true), target,
	)
	if err == nil {
		t.Fatal("a checkpoint from a different PostgreSQL major was accepted: pg_restore would load it into a database it was not dumped from")
	}
	for _, want := range []string{"16", "17", "PostgreSQL"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q, so the operator cannot tell what to run instead: %v", want, err)
		}
	}
}

// TestProvisionalComparisonShowsDifferencesRatherThanReverting asserts the
// property step 4 exists for: a cluster that kept running after the checkpoint
// moved on, so the differences are SHOWN and the live side is not overwritten
// with the checkpoint's values.
func TestProvisionalComparisonShowsDifferencesRatherThanReverting(t *testing.T) {
	const cluster = "prod-2"
	checkpoint := recovery.DesiredState{
		Inventories: map[string]recovery.Fact{
			cluster:      {Value: "revision 3, 2 host(s)"},
			"unchanged":  {Value: "revision 1, 1 host(s)"},
			"registered": {Value: "revision 1, 1 host(s)"},
		},
		Bundles: map[string]recovery.Fact{
			cluster:     {Value: "1.0"},
			"unchanged": {Value: "1.1"},
		},
	}
	// The cluster was scaled after the checkpoint was taken, and its bundle was
	// upgraded; a third cluster registered after it and is unknown to the
	// checkpoint, and a fourth is identical on both sides.
	live := recovery.DesiredState{
		Inventories: map[string]recovery.Fact{
			cluster:     {Value: "revision 5, 3 host(s)"},
			"unchanged": {Value: "revision 1, 1 host(s)"},
			"new":       {Value: "revision 1, 1 host(s)"},
		},
		Bundles: map[string]recovery.Fact{
			cluster:     {Value: "1.1"},
			"unchanged": {Value: "1.1"},
			"new":       {Value: "1.1"},
		},
	}

	diffs := recovery.CompareProvisional(checkpoint, live)
	if len(diffs) == 0 {
		t.Fatal("a cluster that moved on after the checkpoint was reported as agreeing with it")
	}
	seen := map[string]recovery.Difference{}
	for _, d := range diffs {
		seen[d.Cluster+"/"+d.What] = d
	}
	moved, ok := seen[cluster+"/inventory"]
	if !ok {
		t.Fatalf("the scaled cluster's inventory difference was not reported: %v", diffs)
	}
	if moved.Checkpoint == "" || moved.Live == "" {
		t.Fatalf("a difference names only one side (%+v): the operator cannot decide between two states when only one is shown", moved)
	}
	if moved.Live != "revision 5, 3 host(s)" {
		t.Fatalf("the LIVE side is not what the cluster reports: %+v", moved)
	}
	if _, ok := seen[cluster+"/bundle"]; !ok {
		t.Fatalf("the upgraded cluster's bundle difference was not reported: %v", diffs)
	}
	if _, ok := seen["new/inventory"]; !ok {
		t.Fatalf("a cluster registered after the checkpoint was not reported as unknown to it: %v", diffs)
	}
	if _, ok := seen["unchanged/inventory"]; ok {
		t.Fatalf("an identical cluster was reported as a difference: %v", diffs)
	}

	// WHAT IT DOES NOT DO: the checkpoint's value never comes back as the
	// decision. Every difference carries the live side too, and the report says
	// which cluster moved — the caller shows it and pushes nothing.
	var sb strings.Builder
	recovery.Report(&sb, diffs)
	for _, d := range diffs {
		if !strings.Contains(sb.String(), d.Live) {
			t.Fatalf("the report does not show the live side of %s/%s, so an operator cannot see what the cluster actually reports:\n%s", d.Cluster, d.What, sb.String())
		}
	}
	if !strings.Contains(sb.String(), "moved on") && !strings.Contains(sb.String(), "difference") {
		t.Fatalf("the report does not say that the restored state is provisional:\n%s", sb.String())
	}
}

// TestResumeCannotChangeRecoveryIntoInstall is the identity check: a recovery
// journal must not be resumable as an ordinary install, in either direction,
// because the stages the first run completed were doing something else.
func TestResumeCannotChangeRecoveryIntoInstall(t *testing.T) {
	const clusterA = "01a02362-f8a3-7dd6-aa07-2f10ed7a5c23"
	f := newRecoveryFixture(t, clusterA, "org-1", "inst-1", nil)
	path := filepath.Join(t.TempDir(), "journal.json")

	recoveryOpts := recoveryOptions(f, f.fleet.SecretKeyString())
	journal, err := OpenJournal(path, recoveryOpts.Identity())
	if err != nil {
		t.Fatal(err)
	}
	// A stage completed, which is what makes the journal the record of a run
	// that changed something rather than one that never got anywhere.
	if err := journal.Append(Entry{Stage: StageRecoveryOwnership, Status: StatusCompleted, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	// The SAME recovery re-enters its own journal: resume has to keep working.
	if _, err := OpenJournal(path, recoveryOpts.Identity()); err != nil {
		t.Fatalf("a recovery could not resume its own journal: %v", err)
	}

	// An ORDINARY install of the same cluster must not resume it.
	installOpts := Options{Bundle: "1.1", Name: "lost-single-server", Servers: []string{"10.0.9.9"}, HATier: "single-server"}
	_, err = OpenJournal(path, installOpts.Identity())
	if err == nil {
		t.Fatal("a recovery journal was resumed as an ordinary install: the completed stages were doing something else")
	}
	if !strings.Contains(err.Error(), "different") {
		t.Fatalf("the refusal does not say the identity differs: %v", err)
	}
	// The MODE is part of the identity, and the refusal names it: without that,
	// an operator told only "a field differs" would compare servers and bundle
	// versions and never look at the one thing that changed.
	if !strings.Contains(err.Error(), "mode") {
		t.Fatalf("the refusal does not name the mode, which is the field that changed: %v", err)
	}
	if got := recoveryOpts.Identity().Fields["mode"]; got != "recovery-cluster" {
		t.Fatalf("a workload-cluster recovery's journal identity mode is %q", got)
	}
	if got := installOpts.Identity().Fields["mode"]; got != "registered" {
		t.Fatalf("an ordinary install's journal identity mode is %q", got)
	}

	// And the other direction: an install journal must not be resumed as a
	// recovery, which would skip the install's stages and claim a cluster that
	// was never recovered.
	installPath := filepath.Join(t.TempDir(), "install-journal.json")
	installJournal, err := OpenJournal(installPath, installOpts.Identity())
	if err != nil {
		t.Fatal(err)
	}
	if err := installJournal.Append(Entry{Stage: StageK3sServer, Status: StatusCompleted, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJournal(installPath, recoveryOpts.Identity()); err == nil {
		t.Fatal("an ordinary install's journal was resumed as a recovery")
	}
	if recoveryOpts.Identity().Cluster == installOpts.Identity().Cluster {
		t.Fatal("a recovery's journal identity is keyed by the display name: it must be keyed by the immutable cluster id")
	}
}

// TestOwnershipRequiresTheAcknowledgementWhenConditionalWritesFail is the F20
// path: a target that cannot create an object conditionally does not get an
// unconditional write for free — the single-operator rule has to be
// acknowledged, and the acknowledgement is visible in the object.
func TestOwnershipRequiresTheAcknowledgementWhenConditionalWritesFail(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	noConditional := &noConditionalWriter{objects: map[string][]byte{}}
	copy, err := operation.NewCopy(noConditional, recoverykit.Sealer{Recipient: mustRecipient(t)}, recoveryTestScope)
	if err != nil {
		t.Fatal(err)
	}

	// Without the acknowledgement: refused, and NOTHING is written.
	_, _, err = recovery.Claim(context.Background(), copy, "0123456789abcdef0123456789abcdef", "ana@laptop", now, false)
	if err == nil {
		t.Fatal("ownership was taken on a target that cannot create conditionally, with no acknowledgement")
	}
	if !strings.Contains(err.Error(), "acknowledge-single-operator") {
		t.Fatalf("the refusal does not name how to proceed: %v", err)
	}
	if noConditional.puts != 0 {
		t.Fatalf("a refused claim wrote %d object(s)", noConditional.puts)
	}

	// With it: the object is written, and says it was written unquestioned.
	key, mode, err := recovery.Claim(context.Background(), copy, "0123456789abcdef0123456789abcdef", "ana@laptop", now, true)
	if err != nil {
		t.Fatalf("an acknowledged claim was refused: %v", err)
	}
	if mode != recovery.OwnershipAcknowledged {
		t.Fatalf("the mode is %q, so the record cannot say the ownership excludes nobody", mode)
	}
	body, ok := noConditional.objects[key]
	if !ok {
		t.Fatalf("the acknowledged claim wrote nothing at %s", key)
	}
	if !strings.Contains(string(body), "acknowledged") {
		t.Fatalf("the ownership object does not record the acknowledgement: %s", body)
	}
}

// noConditionalWriter is a target without `If-None-Match: *`, which is the case
// probe P3 question 1 is about.
type noConditionalWriter struct {
	objects map[string][]byte
	puts    int
}

func (w *noConditionalWriter) Put(_ context.Context, key string, body []byte) error {
	w.puts++
	w.objects[key] = body
	return nil
}

func (w *noConditionalWriter) PutIfAbsent(_ context.Context, _ string, _ []byte) (bool, error) {
	return false, operation.ErrNoConditionalWrites
}

func mustRecipient(t *testing.T) string {
	t.Helper()
	key, err := recoverykit.GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}
	return key.Recipient()
}

// TestACheckpointIsFoundAtTheBucketRootWhateverTheTargetPrefix is the hardware
// defect of 2026-09-28: the WRITER puts every checkpoint at the bucket root
// under `control-plane/` (`controlplane.NewCheckpointTarget` forces that prefix,
// and the checkpoint principal's policy covers only `<bucket>/control-plane/*`),
// while the READER composed the target's prefix in front of it. Every recovery
// from a --backup-target that carries a prefix — which is every real one — found
// no checkpoint at all.
func TestACheckpointIsFoundAtTheBucketRootWhateverTheTargetPrefix(t *testing.T) {
	f := newRecoveryFixture(t, "01a02362-f8a3-7dd6-aa07-2f10ed7a5c31", "org-1", "inst-1", nil)
	// The bucket holds the checkpoint where the writer puts it: the ROOT.
	sealed, err := recoverykit.SealTo(f.fleet.Recipient(), []byte("the dump"))
	if err != nil {
		t.Fatal(err)
	}
	dir := "control-plane/2026-09-28T003149Z-nightly"
	manifest, err := json.Marshal(map[string]any{
		"key":                   dir,
		"at":                    "2026-09-28T00:31:49Z",
		"control_plane_version": "1.1",
		"management_cluster_id": f.bind.ClusterID,
		"envelope":              map[string]any{"sha256": recoverykit.Digest(sealed), "size_bytes": len(sealed)},
		"dump":                  map[string]any{"postgres_major": 17, "postgres_image": "bitnami/postgresql@sha256:cccc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.bucket.objects[dir+"/manifest.json"] = manifest
	f.bucket.objects[dir+"/control-plane.dump.age"] = sealed

	// The target carries a prefix, which is what made the old reader look in
	// the wrong place. testTarget points at .../ + recoveryTestScope.
	s, _ := recoverySession(t, Options{Bundle: "1.1", Servers: []string{"10.0.9.9"}, HATier: "single-server"})
	s.recoveryStore = f.bucket
	s.recoveryTargetValue = testTarget(t)
	if s.recoveryTargetValue.Prefix == "" {
		t.Fatal("this fixture's --backup-target has no prefix, so the test cannot tell the two compositions apart")
	}
	cp, dump, err := s.selectCheckpoint(context.Background())
	if err != nil {
		t.Fatalf("the checkpoint the writer wrote was not found: %v", err)
	}
	if cp.ControlPlaneVersion != "1.1" || len(dump) == 0 {
		t.Fatalf("the checkpoint was read as %+v (dump %d bytes)", cp, len(dump))
	}
	// The path it looked at is the writer's, not the target's prefix plus it.
	if key := s.checkpointPrefix(); key != strings.Trim(backup.ControlPlanePrefix, "/") {
		t.Fatalf("the reader looks under %q, and the writer writes under %q", key, backup.ControlPlanePrefix)
	}
}
