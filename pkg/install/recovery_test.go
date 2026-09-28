package install

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
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

// onlyThe11Backup leaves only the coverage-less backup in the bucket, which is
// what a bundle 1.1 fleet has: the fixture's own set (whose backup records its
// coverage, as a 1.2 operator does) is removed so the test is about the shape
// the hardware produced.
func onlyThe11Backup(f *recoveryFixture) {
	delete(f.bucket.objects, recoverykit.SetKey(recoveryTestScope, f.bind.ClusterID, recoverykit.KindCluster, f.art))
	delete(f.bucket.objects, recoverykit.KitKey(recoveryTestScope, f.bind.ClusterID, recoverykit.KindCluster, f.art))
}

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
	// The checkpoint lives at the bucket root and is read with the control
	// plane's own principal; this fixture has one store, so it is both.
	s.recoveryCheckpointStore = f.bucket
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

// TestTheCheckpointIsReadWithTheControlPlanesOwnPrincipal is the hardware
// defect of 2026-09-28: the recovery listed the checkpoints with the CLUSTER
// principal, so a store that follows PLAN 7.8's separate-principal rule — the
// cluster's credential scoped to its own prefix, the control plane's to
// `control-plane/` — answered AccessDenied, and no recovery from such a store
// could start.
func TestTheCheckpointIsReadWithTheControlPlanesOwnPrincipal(t *testing.T) {
	f := newRecoveryFixture(t, "01a02362-f8a3-7dd6-aa07-2f10ed7a5c32", "org-1", "inst-1", nil)
	sealed, err := recoverykit.SealTo(f.fleet.Recipient(), []byte("the dump"))
	if err != nil {
		t.Fatal(err)
	}
	dir := "control-plane/2026-09-28T010000Z-nightly"
	manifest, err := json.Marshal(map[string]any{
		"key":                   dir,
		"at":                    "2026-09-28T01:00:00Z",
		"control_plane_version": "1.1",
		"management_cluster_id": f.bind.ClusterID,
		"envelope":              map[string]any{"sha256": recoverykit.Digest(sealed), "size_bytes": len(sealed)},
		"dump":                  map[string]any{"postgres_major": 17, "postgres_image": "bitnami/postgresql@sha256:cccc"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Two stores with two policies, which is what a customer following PLAN 7.8
	// has: the cluster's refuses anything under control-plane/, the control
	// plane's refuses anything outside it.
	clusterStore := &scopedBucket{
		objects: f.bucket.objects,
		allow:   func(key string) bool { return !strings.HasPrefix(key, backup.ControlPlanePrefix) },
	}
	checkpointStore := &scopedBucket{
		objects: map[string][]byte{dir + "/manifest.json": manifest, dir + "/control-plane.dump.age": sealed},
		allow:   func(key string) bool { return strings.HasPrefix(key, backup.ControlPlanePrefix) },
	}

	s, _ := recoverySession(t, Options{Bundle: "1.1", Servers: []string{"10.0.9.9"}, HATier: "single-server"})
	s.recoveryStore = clusterStore
	s.recoveryCheckpointStore = checkpointStore
	s.recoveryTargetValue = testTarget(t)

	cp, dump, err := s.selectCheckpoint(context.Background())
	if err != nil {
		t.Fatalf("the checkpoint was not read with the control plane's own principal: %v", err)
	}
	if cp.ControlPlaneVersion != "1.1" || len(dump) != len(sealed) {
		t.Fatalf("the checkpoint was read as %+v (dump %d bytes)", cp, len(dump))
	}

	// And the other half: with no checkpoint principal the recovery refuses
	// BEFORE reading, naming the variables, rather than surfacing a store's
	// AccessDenied.
	t.Setenv("KUBENEST_CONTROL_PLANE_CHECKPOINT_ACCESS_KEY_ID", "")
	t.Setenv("KUBENEST_CONTROL_PLANE_CHECKPOINT_SECRET_ACCESS_KEY", "")
	t.Setenv("KUBENEST_CHECKPOINT_ACCESS_KEY_ID", "")
	t.Setenv("KUBENEST_CHECKPOINT_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	s.recoveryCheckpointStore = nil
	_, _, err = s.selectCheckpoint(context.Background())
	if err == nil {
		t.Fatal("a recovery with no checkpoint principal tried to read the checkpoints anyway")
	}
	for _, want := range []string{"KUBENEST_CONTROL_PLANE_CHECKPOINT_ACCESS_KEY_ID", "separate"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q, so the operator cannot tell which credential is missing: %v", want, err)
		}
	}
}

// scopedBucket is a store with a policy: it answers AccessDenied to everything
// outside its scope, exactly as the S3-compatible store does.
type scopedBucket struct {
	objects map[string][]byte
	allow   func(key string) bool
}

func (b *scopedBucket) permitted(key string) error {
	if b.allow != nil && !b.allow(key) {
		return fmt.Errorf("s3 ListObjectsV2: AccessDenied (403): Access Denied")
	}
	return nil
}

func (b *scopedBucket) Get(_ context.Context, key string) ([]byte, error) {
	if err := b.permitted(key); err != nil {
		return nil, err
	}
	if body, ok := b.objects[key]; ok {
		return body, nil
	}
	return nil, s3.ErrNotFound
}

func (b *scopedBucket) List(_ context.Context, prefix string) ([]string, bool, error) {
	if err := b.permitted(prefix); err != nil {
		return nil, false, err
	}
	var keys []string
	for key := range b.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, false, nil
}

// TestTheCheckpointStageWaitsForTheDatabasePod is the hardware defect of
// 2026-09-28: `recovery-control-plane` writes the HelmChart and returns, k3s's
// Helm controller installs it asynchronously, and the checkpoint stage looked
// for the database pod at once and refused — three seconds before it existed.
func TestTheCheckpointStageWaitsForTheDatabasePod(t *testing.T) {
	const podLookup = "app.kubernetes.io/instance=kubenest-cp,app.kubernetes.io/name=postgresql"
	const statefulset = `{"spec":{"replicas":1},"status":{"readyReplicas":%d}}`

	// The pod does not exist for the first two observations, and the
	// StatefulSet reports no ready replica until the third.
	podLookups, stsLookups := 0, 0
	runner := &recordingRunner{respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.Contains(command, "get pods") && strings.Contains(command, podLookup):
			podLookups++
			if podLookups < 3 {
				return sshx.Result{Stdout: ""}, nil
			}
			return sshx.Result{Stdout: "kubenest-cp-postgresql-0"}, nil
		case strings.Contains(command, "get statefulset"):
			stsLookups++
			ready := 0
			if stsLookups >= 3 {
				ready = 1
			}
			return sshx.Result{Stdout: fmt.Sprintf(statefulset, ready)}, nil
		}
		return sshx.Result{}, nil
	}}
	s, _ := recoverySession(t, Options{Bundle: "1.1", Servers: []string{"10.0.9.9"}, HATier: "single-server"})
	if err := s.waitForDatabasePod(context.Background(), runner, 10*time.Second, 20*time.Millisecond); err != nil {
		t.Fatalf("the database pod appeared after two polls and the stage gave up: %v", err)
	}
	if podLookups < 3 {
		t.Fatalf("the pod was looked up %d time(s): a single look is what refused a pod that existed three seconds later", podLookups)
	}

	// A pod that never becomes Ready fails at the deadline, and the failure
	// names the last state rather than "timed out".
	stuck := &recordingRunner{respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.Contains(command, "get pods") && strings.Contains(command, podLookup):
			return sshx.Result{Stdout: "kubenest-cp-postgresql-0"}, nil
		case strings.Contains(command, "get statefulset"):
			return sshx.Result{Stdout: `{"spec":{"replicas":1},"status":{"readyReplicas":0}}`}, nil
		case strings.Contains(command, "get pod kubenest-cp-postgresql-0"):
			return sshx.Result{Stdout: `{"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"0/1 nodes are available: 1 Insufficient storage"}],"containerStatuses":[{"state":{"waiting":{"reason":"ContainerCreating"}}}]}}`}, nil
		}
		return sshx.Result{}, nil
	}}
	err := s.waitForDatabasePod(context.Background(), stuck, 200*time.Millisecond, 20*time.Millisecond)
	if err == nil {
		t.Fatal("a database pod that never became Ready let the restore run")
	}
	for _, want := range []string{"0/1 Ready", "Unschedulable"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the failure does not name %q, so a stuck claim reads as an unexplained timeout: %v", want, err)
		}
	}

	// And a wait with no deadline is refused rather than defaulted: every wait
	// in this CLI is bounded by the bundle's limits.
	if err := s.waitForDatabasePod(context.Background(), runner, 0, time.Second); err == nil {
		t.Fatal("waiting for the database pod with no deadline was accepted")
	}
}

// TestTheOwnershipTestSurvivesAFailedLaterAttempt is the measured journal shape
// of the 2026-09-28 hardware run: the first attempt completed k3s-server and
// recovery-control-plane, and the two resumes that followed FAILED
// recovery-control-plane (on the create-only Secret and then on the all-fields
// comparison). The journal therefore no longer records that stage as completed,
// and a resume that asked "did recovery-control-plane complete?" would refuse
// its own Secret for ever.
func TestTheOwnershipTestSurvivesAFailedLaterAttempt(t *testing.T) {
	f := newRecoveryFixture(t, "01a02362-f8a3-7dd6-aa07-2f10ed7a5c33", "org-1", "inst-1", nil)
	opts := Options{
		Bundle: "1.1", Servers: []string{"10.0.9.9"}, HATier: "single-server",
		Recovery: &RecoveryOptions{
			RestoreFrom: "latest", Kit: "s3", FleetKey: f.fleet.SecretKeyString(),
			OldHostFenced: true, Kind: recoverykit.KindControlPlane,
		},
		ControlPlaneInstall: true, Name: "prod-1", Domain: "example.test",
	}
	s, _ := recoverySession(t, opts)
	now := time.Now().UTC()
	// The first attempt: it built the host and applied the chart.
	for _, stage := range []string{StageK3sServer, StageRecoveryControlPlane} {
		if err := s.Jnl.Append(Entry{Stage: stage, Status: StatusCompleted, At: now}); err != nil {
			t.Fatal(err)
		}
	}
	if !s.recoveryOwnsHost() {
		t.Fatal("a journal that installed this host's k3s does not count as owning the cluster")
	}
	// The resumes that failed this stage afterwards must not change that.
	if err := s.Jnl.Append(Entry{Stage: StageRecoveryControlPlane, Status: StatusFailed, At: now.Add(time.Minute), Detail: "secret already exists"}); err != nil {
		t.Fatal(err)
	}
	if _, completed := s.Jnl.Completed(StageRecoveryControlPlane); completed {
		t.Fatal("this fixture does not reproduce the hardware shape: the failed attempt left the stage recorded as completed")
	}
	if !s.recoveryOwnsHost() {
		t.Fatal("a failed later attempt cost this operation ownership of the cluster it built, so a resume would refuse its own install Secret")
	}

	// And a journal that never installed k3s does NOT own anything: a first run
	// finding a Secret on a host it did not build must refuse it.
	fresh, _ := recoverySession(t, opts)
	if fresh.recoveryOwnsHost() {
		t.Fatal("an operation that never installed this host's k3s claimed to own the objects in its cluster")
	}
}

// TestAResumedRecoveryRebuildsEveryPieceOfInMemoryState is the hardware defect
// of 2026-09-28 (the second one of that shape): `recovery-preflight` selects the
// checkpoint IN MEMORY, the journal skips the stage on a resume, and the stage
// that loads the database then found no selection. Every piece of state a later
// stage reads from `s.` must therefore be filled by a stage that ALWAYS runs.
func TestAResumedRecoveryRebuildsEveryPieceOfInMemoryState(t *testing.T) {
	f := newRecoveryFixture(t, "01a02362-f8a3-7dd6-aa07-2f10ed7a5c34", "org-1", "inst-1", nil)
	cpOpts := Options{
		Bundle: "1.1", Servers: []string{"10.0.9.9"}, HATier: "single-server",
		ControlPlaneInstall: true, Name: "prod-1", Domain: "example.test",
		Recovery: &RecoveryOptions{
			RestoreFrom: "latest", Kit: "s3", FleetKey: f.fleet.SecretKeyString(),
			OldHostFenced: true, Kind: recoverykit.KindControlPlane,
		},
	}
	s, _ := recoverySession(t, cpOpts)
	// The journal shape of a resume that failed at the checkpoint stage: the
	// host was built, the chart applied, and the loading stage is re-entered.
	now := time.Now().UTC()
	for _, stage := range []string{StageK3sServer, StageRecoveryControlPlane, StageRecoveryRepository} {
		if err := s.Jnl.Append(Entry{Stage: stage, Status: StatusCompleted, At: now}); err != nil {
			t.Fatal(err)
		}
	}
	plan := Plan(s)
	if len(plan) == 0 {
		t.Fatal("the control-plane recovery has no plan")
	}

	// EVERY stage that fills state a later stage reads must be AlwaysRun. The
	// list is the audit: select (the sets, the stores, the kit), the pre-flight
	// (the checkpoint and its dump, and the capacity verdict), the chart stage
	// (the chart values), and the API stage (the client the register stage
	// registers through).
	for _, name := range []string{StageRecoverySelect, StageRecoveryPreflight, StageRecoveryControlPlane, StageRecoveryAPI, StageRegister, StageVerify} {
		idx := stageIndex(t, plan, name)
		if !plan[idx].AlwaysRun {
			t.Fatalf("stage %q fills in-memory state a later stage reads, and the journal would skip it on a resume", name)
		}
	}
	// And the skippable stages must not be the only writer of anything: the
	// ownership stage's operation id and the selection are journalled instead,
	// because those stages may legitimately be skipped.
	if s.recoveryActivationValue() != "" {
		t.Fatal("a session with no journal state claims an activation value")
	}
	s.Record.RecoveryOperationID = "0123456789abcdef0123456789abcdef"
	if got := s.recoveryActivationValue(); got != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("the activation value is not the operation the journal recorded (%q): a resumed activation would write an annotation with an empty value", got)
	}
}

// TestAResumeKeepsTheArtifactTheOperationChose: the bucket may have changed
// between attempts, and a recovery that silently switched to a newer set or
// backup half way through would leave the data it had already restored coming
// from one artifact and the rest from another.
func TestAResumeKeepsTheArtifactTheOperationChose(t *testing.T) {
	f := newRecoveryFixture(t, "01a02362-f8a3-7dd6-aa07-2f10ed7a5c35", "org-1", "inst-1", nil)
	s, _ := recoverySession(t, recoveryOptions(f, f.fleet.SecretKeyString()))
	s.Record.RecoverySetKey = "clusters/s6/recovery-sets/other/cluster-someone-elses.json"
	// The set this attempt would otherwise pick, from the bucket it reads.
	sel, err := recovery.Select(context.Background(), f.bucket, recoveryTestScope, f.request())
	if err != nil {
		t.Fatal(err)
	}
	s.recoverySel = sel
	if err := s.refuseASwitchedSet(sel); err == nil {
		t.Fatal("a resume switched to a different recovery set than the one its earlier attempt used")
	} else if !strings.Contains(err.Error(), "earlier attempt") {
		t.Fatalf("the refusal does not say why it will not switch: %v", err)
	}
	// The same operation resuming its own selection passes, so the check is
	// about a CHANGE and not about resuming at all.
	s.Record.RecoverySetKey = sel.SetKey
	if err := s.refuseASwitchedSet(sel); err != nil {
		t.Fatalf("a resume of the same selection was refused: %v", err)
	}
	// And a first attempt has nothing recorded to disagree with.
	fresh, _ := recoverySession(t, recoveryOptions(f, f.fleet.SecretKeyString()))
	if err := fresh.refuseASwitchedSet(sel); err != nil {
		t.Fatalf("a first attempt was refused: %v", err)
	}
}

// TestA11FleetRecoversItsControlPlaneAndSkipsTheRestore is the hardware dead
// end of 2026-09-28: prod-1 ran bundle 1.1, whose operator (2.6.17) records no
// expected coverage — that arrived with kn-t210 in 2.7.0-rc.1 — so on a 1.1
// fleet no backup can ever carry coverage, and stage 20 refused after the
// control plane was already back, with a resume unable to change it.
//
// The decision (PLAN 7.5's rule, and PLAN 7.9's step 5): never restore data
// whose coverage cannot be shown, and never dead-end a recovery whose control
// plane is already back. The backup is chosen with its reason, the stages SKIP,
// the reason is journalled and reported, and the command finishes.
func TestA11FleetRecoversItsControlPlaneAndSkipsTheRestore(t *testing.T) {
	const clusterA = "01a02362-f8a3-7dd6-aa07-2f10ed7a5c36"
	// A 1.1-shaped bucket: completed, and with no coverage recorded.
	f := newRecoveryFixture(t, clusterA, "org-1", "inst-1", nil)
	onlyThe11Backup(f)
	f.publish(t, f.bind, "20260928T000000Z-11111111", f.secrets, nil, true, recoverykit.Backup{
		Name:        "manual-20260927-235954",
		CompletedAt: time.Date(2026, 9, 27, 23, 59, 54, 0, time.UTC),
		Status:      "Completed",
	})
	sel, err := recovery.SelectManagementCluster(context.Background(), f.bucket, recoveryTestScope, "inst-1", clusterA, "")
	if err != nil {
		t.Fatalf("a 1.1-shaped workload backup was refused at selection: %v", err)
	}
	if sel.Backup.Name != "manual-20260927-235954" {
		t.Fatalf("the backup chosen is %q", sel.Backup.Name)
	}
	if sel.RestoreSkipReason == "" {
		t.Fatal("a backup that records no coverage was chosen with no reason to skip it, so the restore stage would try it")
	}
	for _, want := range []string{"coverage", "kn-t210"} {
		if !strings.Contains(sel.RestoreSkipReason, want) {
			t.Fatalf("the reason does not name %q, so the operator cannot tell why their workloads did not come back: %s", want, sel.RestoreSkipReason)
		}
	}

	// The restore stage SKIPS rather than failing, and records it.
	s, runner := recoverySession(t, recoveryOptions(f, f.fleet.SecretKeyString()))
	s.recoverySel = sel
	s.Record.RecoveryRestoreSkipReason = sel.RestoreSkipReason
	s.Record.RecoveryBackupName = sel.Backup.Name
	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("the stage failed on a backup it had already decided not to restore from, which is the dead end this fix removes: %v", err)
	}
	if len(s.Record.RecoveryNamespacesNotRestored) != 0 {
		t.Fatalf("a backup that names no namespaces produced a namespace list: %v", s.Record.RecoveryNamespacesNotRestored)
	}
	if len(runner.ran) != 0 {
		t.Fatalf("the skip issued %d command(s) on the cluster: %v", len(runner.ran), runner.ran)
	}

	// Activation leaves the workloads held rather than starting them against
	// whatever is in the namespace.
	if err := stageRecoveryActivate(context.Background(), s); err != nil {
		t.Fatalf("activation failed: %v", err)
	}
	if runner.ranAny("annotate") {
		t.Fatalf("a namespace whose data was not restored was activated: %s", runner.ranAll())
	}
}

// TestAnEligibleBackupStillRestores: the skip is for a backup nobody can show
// the coverage of, not for the recovery path in general. A backup that records
// its namespaces is chosen with no reason to skip.
func TestAnEligibleBackupStillRestores(t *testing.T) {
	f := newRecoveryFixture(t, "01a02362-f8a3-7dd6-aa07-2f10ed7a5c37", "org-1", "inst-1", nil)
	sel, err := recovery.SelectManagementCluster(context.Background(), f.bucket, recoveryTestScope, "inst-1", f.bind.ClusterID, "")
	if err != nil {
		t.Fatal(err)
	}
	if sel.RestoreSkipReason != "" {
		t.Fatalf("a backup that records its coverage was skipped: %s", sel.RestoreSkipReason)
	}
	if len(sel.Backup.Coverage) == 0 {
		t.Fatal("the fixture's backup records no coverage, so this test cannot tell the two cases apart")
	}
}

// TestTheResumeFromAFailedStage20Completes: stage 20 failed on the hardware, so
// the journal has it failed and everything before it completed or re-run. With
// the reason recorded, the re-entered restore stage finishes instead of failing
// again, and the stages after it run.
func TestTheResumeFromAFailedStage20Completes(t *testing.T) {
	const clusterA = "01a02362-f8a3-7dd6-aa07-2f10ed7a5c38"
	f := newRecoveryFixture(t, clusterA, "org-1", "inst-1", nil)
	onlyThe11Backup(f)
	f.publish(t, f.bind, "20260928T010000Z-22222222", f.secrets, nil, true, recoverykit.Backup{
		Name: "manual-20260927-235954", CompletedAt: time.Date(2026, 9, 27, 23, 59, 54, 0, time.UTC), Status: "Completed",
	})
	sel, err := recovery.SelectManagementCluster(context.Background(), f.bucket, recoveryTestScope, "inst-1", clusterA, "")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := recoverySession(t, recoveryOptions(f, f.fleet.SecretKeyString()))
	s.recoverySel = sel
	now := time.Now().UTC()
	for _, stage := range []string{StageRecoveryOwnership, StageK3sServer, StageRecoveryRepository, StageRegister, StageAgent, StageRecoveryProvisional} {
		if err := s.Jnl.Append(Entry{Stage: stage, Status: StatusCompleted, At: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Jnl.Append(Entry{Stage: StageRecoveryRestore, Status: StatusFailed, At: now, Detail: "records no namespace coverage"}); err != nil {
		t.Fatal(err)
	}
	// The selection the resume re-derives carries the reason, the stage records
	// it and returns, and the activation and report stages have what they need.
	s.Record.RecoveryRestoreSkipReason = sel.RestoreSkipReason
	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("the resumed stage failed again: %v", err)
	}
	if err := s.saveRecord(); err != nil {
		t.Fatal(err)
	}
	record, err := Recorded(s.Jnl)
	if err != nil {
		t.Fatal(err)
	}
	if record.RecoveryRestoreSkipReason == "" {
		t.Fatal("the reason was not journalled, so the report cannot state it after a resume")
	}
}

// TestTheControlPlaneSelectCarriesTheSkipReasonToo: the S6 path carried the
// reason and the control-plane path did not, so an all-in-one recovery reached
// stage 20 with an empty reason and refused exactly as the S6 path had before
// the fix. Both select paths must set every field the other does.
//
// The parity is asserted field by field rather than by reading the code: a
// selection that decides something the later stages act on has to survive being
// made by either path.
func TestTheControlPlaneSelectCarriesTheSkipReasonToo(t *testing.T) {
	const mgmt = "01a02362-f8a3-7dd6-aa07-2f10ed7a5c39"
	f := newRecoveryFixture(t, mgmt, "org-1", "inst-1", nil)
	onlyThe11Backup(f)
	f.publish(t, f.bind, "20260928T020000Z-33333333", f.secrets, nil, true, recoverykit.Backup{
		Name: "manual-20260927-235954", CompletedAt: time.Date(2026, 9, 27, 23, 59, 54, 0, time.UTC), Status: "Completed",
	})
	// The instance's own set, beside it, as a --control-plane install writes it.
	cpBinding := recoverykit.Binding{Kind: recoverykit.KindControlPlane, InstanceID: "inst-1", ClusterID: mgmt}
	f.publish(t, cpBinding, "20260928T020000Z-44444444", map[string]string{
		recoverykit.KeyEncryptionKey:  "VALUE-enc",
		recoverykit.KeyAgentJWTSecret: "VALUE-jwt",
		recoverykit.KeyControlPlaneCA: "-----BEGIN CERTIFICATE-----\nY2VydA==\n-----END CERTIFICATE-----\n-----BEGIN EC PRIVATE KEY-----\nYTJiM2M0\n-----END EC PRIVATE KEY-----\n",
	}, nil, true, recoverykit.Backup{
		Name: "cp-baseline", CompletedAt: time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC), Status: "Completed",
	})

	opts := Options{
		Bundle: "1.1", Servers: []string{"10.0.9.9"}, HATier: "single-server",
		ControlPlaneInstall: true, Name: "prod-1", Domain: "example.test",
		BackupTarget: "s3://kubenest-kit/" + recoveryTestScope + "?endpoint=minio.example.test&region=main",
		Recovery: &RecoveryOptions{
			RestoreFrom: "latest", Kit: "s3", FleetKey: f.fleet.SecretKeyString(),
			OldHostFenced: true, Kind: recoverykit.KindControlPlane,
		},
	}
	s, _ := recoverySession(t, opts)
	s.recoveryStore = f.bucket
	s.recoveryCheckpointStore = f.bucket
	s.recoveryTargetValue = testTarget(t)
	if err := stageRecoverySelectControlPlane(context.Background(), s); err != nil {
		t.Fatalf("the control-plane select stage failed: %v", err)
	}
	if s.recoveryWorkloadSel == nil {
		t.Fatal("the control-plane select did not select the management cluster's own set")
	}
	if s.Record.RecoveryRestoreSkipReason == "" {
		t.Fatal("the control-plane select did not record why the workloads cannot be restored, so stage 20 refuses with the old dead end")
	}
	if s.Record.RecoverySetKey == "" || s.Record.RecoveryBackupName == "" {
		t.Fatalf("the control-plane select did not record the artifacts it chose: set %q, backup %q", s.Record.RecoverySetKey, s.Record.RecoveryBackupName)
	}
	// And stage 20 acts on it: it finishes rather than refusing.
	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("stage 20 refused on a recovery whose select stage had already decided to skip: %v", err)
	}
}

// recoveryRegisterAPI answers the register stage for these tests — both the
// ordinary path's calls and the recovery path's — and keeps the ORDER of the
// calls it saw.
//
// THE TRANSCRIPT IS THE EVIDENCE. "A new incarnation is recorded" and "it is
// recorded before the credentials are minted" are claims about what crossed the
// wire and in which order, so both are read from the calls the client made
// rather than from the stage's own log lines.
type recoveryRegisterAPI struct {
	// cluster is the record every path reads: by id for the recovery path, and
	// out of the organisation's cluster list by name for the ordinary one.
	cluster *api.Cluster

	mu       sync.Mutex
	calls    []string
	recorded []recordedIncarnation
	mints    int
}

// recordedIncarnation is one POST to the incarnations endpoint, with what it
// carried.
type recordedIncarnation struct {
	clusterID string
	reason    string
	note      string
}

func (a *recoveryRegisterAPI) serve(t *testing.T) *api.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		a.mu.Lock()
		a.calls = append(a.calls, req.Method+" "+req.URL.Path)
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		incarnations := "/api/v1/clusters/" + a.cluster.ID + "/incarnations"
		credentials := "/api/v1/clusters/" + a.cluster.ID + "/agent-credentials"

		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/api/v1/orgs":
			a.write(w, []api.Org{{ID: a.cluster.OrgID, Name: "Acme", Slug: "acme"}})
		case req.Method == http.MethodGet && req.URL.Path == "/api/v1/orgs/"+a.cluster.OrgID+"/clusters":
			// The listing is paginated: a bare array here is not what the
			// client reads, and the envelope is what tells it there is no
			// second page.
			a.write(w, map[string]any{"data": []api.Cluster{*a.cluster}, "has_more": false, "total_count": 1, "page": 1})
		case req.Method == http.MethodGet && req.URL.Path == "/api/v1/clusters/"+a.cluster.ID:
			a.write(w, a.cluster)
		case req.Method == http.MethodPost && req.URL.Path == incarnations:
			var body struct {
				Reason string `json:"reason"`
				Note   string `json:"note"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Errorf("reading the incarnation request: %v", err)
			}
			a.mu.Lock()
			a.recorded = append(a.recorded, recordedIncarnation{clusterID: a.cluster.ID, reason: body.Reason, note: body.Note})
			ordinal := len(a.recorded)
			a.mu.Unlock()
			a.write(w, map[string]any{
				"id": fmt.Sprintf("inc-%d", ordinal), "cluster_id": a.cluster.ID,
				"ordinal": ordinal, "reason": body.Reason,
				"recorded_at": time.Now().UTC().Format(time.RFC3339),
			})
		case req.Method == http.MethodPost && req.URL.Path == credentials:
			a.mu.Lock()
			a.mints++
			version := a.mints + 1
			a.mu.Unlock()
			a.write(w, map[string]any{
				"cluster_id": a.cluster.ID,
				"agent_jwt": map[string]any{
					"token": "agent-token", "hub_url": "wss://hub.example.test/ws/operator",
					"expires_at":    time.Date(2027, 9, 26, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
					"token_version": version,
				},
				"operator": map[string]any{"namespace": "kubenest-system", "chart_ref": "agent-1.1.0"},
			})
		default:
			t.Errorf("the register stage called %s %s, which this fake does not answer: the test would otherwise pass on a path it never served", req.Method, req.URL.Path)
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := api.New(srv.URL, api.WithToken("knp_test"))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return client
}

func (a *recoveryRegisterAPI) write(w http.ResponseWriter, body any) {
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The handler runs on the server's goroutine, so there is no test to
		// fail from here; the client sees a broken response instead.
		return
	}
}

// incarnations is every incarnation this fake was asked to record.
func (a *recoveryRegisterAPI) incarnations() []recordedIncarnation {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]recordedIncarnation(nil), a.recorded...)
}

// minted is how many credential mints happened.
func (a *recoveryRegisterAPI) minted() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mints
}

// callIndex is where the first call carrying a fragment sits in the transcript,
// or -1. It is what makes "the incarnation comes first" checkable.
func (a *recoveryRegisterAPI) callIndex(fragment string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, call := range a.calls {
		if strings.Contains(call, fragment) {
			return i
		}
	}
	return -1
}

// controlPlaneRegisterSession is an all-in-one recovery's session at the moment
// its register stage runs: the select stage has run, so the sets, the management
// cluster's id and the control-plane client are all in place, and the plan is
// the one the command would run.
func controlPlaneRegisterSession(t *testing.T, f *recoveryFixture, client *api.Client) *Session {
	t.Helper()
	opts := Options{
		Bundle: "1.1", Servers: []string{"10.0.9.9"}, HATier: "single-server",
		ControlPlaneInstall: true, Name: "prod-1", Domain: "example.test",
		InstanceID:   "inst-1",
		BackupTarget: "s3://kubenest-kit/" + recoveryTestScope + "?endpoint=minio.example.test&region=main",
		Recovery: &RecoveryOptions{
			RestoreFrom: "latest", Kit: "s3", FleetKey: f.fleet.SecretKeyString(),
			OldHostFenced: true, Kind: recoverykit.KindControlPlane,
		},
	}
	s, _ := recoverySession(t, opts)
	s.API = client
	s.recoveryStore = f.bucket
	s.recoveryCheckpointStore = f.bucket
	s.recoveryTargetValue = testTarget(t)
	if err := stageRecoverySelectControlPlane(context.Background(), s); err != nil {
		t.Fatalf("the control-plane select stage failed: %v", err)
	}
	if s.Jnl.ClusterID != f.bind.ClusterID {
		t.Fatalf("the select stage bound the session to cluster %s and this fixture is about %s", s.Jnl.ClusterID, f.bind.ClusterID)
	}
	return s
}

// publishControlPlaneSet writes the instance's own recovery set beside the
// management cluster's, as a --control-plane install does. Its binding carries
// NO organisation on purpose: a control-plane kit belongs to an instance, and
// that is why the set which can confirm a cluster record is the cluster's own.
func publishControlPlaneSet(t *testing.T, f *recoveryFixture, artifact string) {
	t.Helper()
	f.publish(t, recoverykit.Binding{Kind: recoverykit.KindControlPlane, InstanceID: "inst-1", ClusterID: f.bind.ClusterID},
		artifact, map[string]string{
			recoverykit.KeyEncryptionKey:  "VALUE-enc",
			recoverykit.KeyAgentJWTSecret: "VALUE-jwt",
			recoverykit.KeyControlPlaneCA: "-----BEGIN CERTIFICATE-----\nY2VydA==\n-----END CERTIFICATE-----\n-----BEGIN EC PRIVATE KEY-----\nYTJiM2M0\n-----END EC PRIVATE KEY-----\n",
		}, nil, true, recoverykit.Backup{
			Name: "cp-baseline", CompletedAt: time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC), Status: "Completed",
		})
}

// TestTheAllInOneRegisterRecordsANewIncarnationBeforeMinting is the hardware
// defect of 2026-09-28 on lab s11: the all-in-one recovery registered the
// management cluster "by the normal path", which records no incarnation — and
// the backend re-delivers a cluster's confirmed projects only when a NEW
// incarnation of that cluster is recorded, so stage 20 waited its full ten
// minutes for Project kubenest-system/s11-data and failed with "never arrived
// on the rebuilt cluster".
//
// THE ORDER IS AS LOAD-BEARING AS THE RECORD. Recording the incarnation raises
// the cluster's revocation floor, so an incarnation recorded AFTER the mint
// would refuse the token the mint had just issued.
func TestTheAllInOneRegisterRecordsANewIncarnationBeforeMinting(t *testing.T) {
	const mgmt = "01a02362-f8a3-7dd6-aa07-2f10ed7a5c41"
	f := newRecoveryFixture(t, mgmt, "org-1", "inst-1", nil)
	publishControlPlaneSet(t, f, "20260928T040000Z-66666666")
	fake := &recoveryRegisterAPI{cluster: &api.Cluster{ID: mgmt, Name: "prod-1", OrgID: "org-1", Status: "connected"}}
	s := controlPlaneRegisterSession(t, f, fake.serve(t))

	plan := Plan(s)
	if err := plan[stageIndex(t, plan, StageRegister)].Run(context.Background()); err != nil {
		t.Fatalf("the all-in-one recovery's register stage failed: %v", err)
	}

	incarnations := fake.incarnations()
	if len(incarnations) != 1 {
		t.Fatalf("the all-in-one register recorded %d incarnation(s) of the management cluster, want 1: without one the control plane has no boundary to re-arm against, and the projects the restored database holds are never re-delivered to the rebuilt cluster", len(incarnations))
	}
	got := incarnations[0]
	if want := s.recoveryWorkloadSel.Set.Binding.ClusterID; got.clusterID != want {
		t.Fatalf("the incarnation was recorded for cluster %s and the management cluster's own set is bound to %s: the identity is the CLUSTER's, and rebuilding a host must not register some other cluster", got.clusterID, want)
	}
	if got.reason != "recovery" {
		t.Fatalf("the incarnation was recorded for reason %q: a rebuild is the one reason the control plane retires the superseded machine's credentials for", got.reason)
	}
	// The detail names the artifact and the host, like the workload path's.
	if key := s.recoveryWorkloadSel.SetKey; !strings.Contains(got.note, key) {
		t.Fatalf("the incarnation's detail (%q) does not name the recovery set this rebuild came from (%s), so the control plane's record does not say what the new machine was built from", got.note, key)
	}
	if !strings.Contains(got.note, "10.0.9.9") {
		t.Fatalf("the incarnation's detail (%q) does not name the host the cluster was rebuilt on", got.note)
	}

	// THE INCARNATION COMES FIRST, and the credentials are minted once.
	recorded, minted := fake.callIndex("/incarnations"), fake.callIndex("/agent-credentials")
	if minted < 0 {
		t.Fatal("the register stage minted no credentials, so the rebuilt agent has nothing to report with")
	}
	if recorded > minted {
		t.Fatalf("the incarnation was recorded at call %d and the credentials minted at call %d: the mint sets the revocation floor, so recording the incarnation afterwards would refuse the token this very mint issued", recorded, minted)
	}
	if n := fake.minted(); n != 1 {
		t.Fatalf("the register stage minted %d time(s), want exactly 1", n)
	}
	if s.Record.TokenVersion != 2 {
		t.Fatalf("the minted credentials were not adopted by the session (token version %d, want the fake's 2), so the agent stage has nothing to install", s.Record.TokenVersion)
	}
}

// TestTheAllInOneRegisterRefusesARecordFromAnotherOrganisation is the planted
// negative the incarnation record needs: adopting a cluster record whose
// organisation is not the one the recovery set is bound to would rebuild
// ANOTHER cluster's host with this cluster's data.
//
// It also says which set does the confirming. The instance's own control-plane
// set carries no organisation at all — a control-plane kit belongs to an
// instance — so the authority here is the management cluster's own set, whose
// binding does carry it.
func TestTheAllInOneRegisterRefusesARecordFromAnotherOrganisation(t *testing.T) {
	const mgmt = "01a02362-f8a3-7dd6-aa07-2f10ed7a5c42"
	f := newRecoveryFixture(t, mgmt, "org-1", "inst-1", nil)
	publishControlPlaneSet(t, f, "20260928T050000Z-77777777")
	foreign := &recoveryRegisterAPI{cluster: &api.Cluster{ID: mgmt, Name: "prod-1", OrgID: "org-2", Status: "connected"}}
	s := controlPlaneRegisterSession(t, f, foreign.serve(t))

	plan := Plan(s)
	err := plan[stageIndex(t, plan, StageRegister)].Run(context.Background())
	if err == nil {
		t.Fatal("a management cluster record in another organisation was adopted: this recovery set belongs to org-1 and the record it registers is org-2's")
	}
	if !strings.Contains(err.Error(), "org-2") || !strings.Contains(err.Error(), "org-1") {
		t.Fatalf("the refusal does not name both organisations, so the operator cannot tell which side is wrong: %v", err)
	}
	if n := len(foreign.incarnations()); n != 0 {
		t.Fatalf("the refusal recorded %d incarnation(s) anyway: a refused adoption must not register a new machine for the cluster it refused", n)
	}
	if n := foreign.minted(); n != 0 {
		t.Fatalf("the refusal minted %d credential set(s) anyway: credentials minted for a refused adoption would rotate the real cluster's identity", n)
	}
}

// TestAResumedAllInOneRegisterRecordsNoSecondIncarnation: the resume after a
// failed later stage must not record a second incarnation or re-mint. The
// operator is already installed and holding the credentials the first attempt
// minted, so a second mint would bump the token version again and raise the
// cluster's revocation floor above the token that machine is using.
func TestAResumedAllInOneRegisterRecordsNoSecondIncarnation(t *testing.T) {
	const mgmt = "01a02362-f8a3-7dd6-aa07-2f10ed7a5c43"
	f := newRecoveryFixture(t, mgmt, "org-1", "inst-1", nil)
	publishControlPlaneSet(t, f, "20260928T060000Z-88888888")
	fake := &recoveryRegisterAPI{cluster: &api.Cluster{ID: mgmt, Name: "prod-1", OrgID: "org-1", Status: "connected"}}
	s := controlPlaneRegisterSession(t, f, fake.serve(t))
	plan := Plan(s)
	register := plan[stageIndex(t, plan, StageRegister)]

	// The first attempt: one incarnation, one mint.
	if err := register.Run(context.Background()); err != nil {
		t.Fatalf("the first attempt's register stage failed: %v", err)
	}
	if n := len(fake.incarnations()); n != 1 {
		t.Fatalf("the first attempt recorded %d incarnation(s), want 1: this test cannot tell a resume from a first attempt unless the first attempt records one", n)
	}
	if n := fake.minted(); n != 1 {
		t.Fatalf("the first attempt minted %d time(s), want 1", n)
	}

	// The resume: a later stage failed, and the agent stage had already
	// completed, so the credentials the first attempt minted are in use.
	if err := s.Jnl.Append(Entry{Stage: StageAgent, Status: StatusCompleted, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := register.Run(context.Background()); err != nil {
		t.Fatalf("the resumed register stage failed: %v", err)
	}
	if n := len(fake.incarnations()); n != 1 {
		t.Fatalf("the resume recorded a second incarnation (now %d): the floor would rise above the token the machine is already using", n)
	}
	if n := fake.minted(); n != 1 {
		t.Fatalf("the resume re-minted (now %d): the operator installed by the first attempt holds the older token, and the mint would refuse it", n)
	}
}

// TestAWorkloadRecoveryStillRecordsItsIncarnation is the control for the fix:
// the workload path's register stage is the shared one, and what it does — one
// incarnation for the cluster's own id, reason "recovery", before the mint —
// must be what it did before the all-in-one path started using it.
func TestAWorkloadRecoveryStillRecordsItsIncarnation(t *testing.T) {
	const clusterA = "01a02362-f8a3-7dd6-aa07-2f10ed7a5c44"
	f := newRecoveryFixture(t, clusterA, "org-1", "inst-1", nil)
	fake := &recoveryRegisterAPI{cluster: &api.Cluster{ID: clusterA, Name: "prod-1", OrgID: "org-1", Status: "connected"}}
	s, _ := recoverySession(t, recoveryOptions(f, f.fleet.SecretKeyString()))
	s.API = fake.serve(t)
	s.recoveryStore = f.bucket
	sel, err := recovery.Select(context.Background(), f.bucket, recoveryTestScope, f.request())
	if err != nil {
		t.Fatal(err)
	}
	s.recoverySel = sel
	s.Jnl.ClusterID = clusterA

	plan := Plan(s)
	if err := plan[stageIndex(t, plan, StageRegister)].Run(context.Background()); err != nil {
		t.Fatalf("the workload recovery's register stage failed: %v", err)
	}

	incarnations := fake.incarnations()
	if len(incarnations) != 1 {
		t.Fatalf("the workload recovery recorded %d incarnation(s), want 1: a rebuilt workload cluster is a new machine under the same immutable id, and its projects are re-delivered on that boundary", len(incarnations))
	}
	if incarnations[0].clusterID != clusterA || incarnations[0].reason != "recovery" {
		t.Fatalf("the incarnation was recorded for cluster %s with reason %q, want %s and \"recovery\"", incarnations[0].clusterID, incarnations[0].reason, clusterA)
	}
	if !strings.Contains(incarnations[0].note, sel.SetKey) {
		t.Fatalf("the incarnation's detail (%q) does not name the recovery set this rebuild came from (%s)", incarnations[0].note, sel.SetKey)
	}
	if recorded, minted := fake.callIndex("/incarnations"), fake.callIndex("/agent-credentials"); recorded > minted || minted < 0 {
		t.Fatalf("the incarnation was recorded at call %d and the credentials minted at call %d: the incarnations must come first", recorded, minted)
	}
}

// TestAnAlwaysRunStageDoesNotUndoWhatALaterStageEstablished is the class the
// hardware found on 2026-09-28: stage 10 (AlwaysRun) re-applied the chart with
// the backend HELD, stage 11 (skippable, and already completed) is what starts
// it, and every resume after that left the backend stopped — so recovery-api
// could not log in and no later stage had anything to talk to.
//
// The fix renders the replica count from the DATABASE rather than from this
// stage's memory, which also keeps the content-addressed revision stable across
// resumes: the same database state renders the same values.
func TestAnAlwaysRunStageDoesNotUndoWhatALaterStageEstablished(t *testing.T) {
	const mgmt = "01a02362-f8a3-7dd6-aa07-2f10ed7a5c40"
	build := func(t *testing.T, dbRows string, psqlErr string) (*Session, *recordingRunner) {
		t.Helper()
		f := newRecoveryFixture(t, mgmt, "org-1", "inst-1", nil)
		cpBinding := recoverykit.Binding{Kind: recoverykit.KindControlPlane, InstanceID: "inst-1", ClusterID: mgmt}
		f.publish(t, cpBinding, "20260928T030000Z-55555555", map[string]string{
			recoverykit.KeyEncryptionKey:  "VALUE-enc",
			recoverykit.KeyAgentJWTSecret: "VALUE-jwt",
			recoverykit.KeyControlPlaneCA: "-----BEGIN CERTIFICATE-----\nY2VydA==\n-----END CERTIFICATE-----\n-----BEGIN EC PRIVATE KEY-----\nYTJiM2M0\n-----END EC PRIVATE KEY-----\n",
		}, nil, true, recoverykit.Backup{Name: "cp-baseline", CompletedAt: time.Now().UTC(), Status: "Completed"})
		opts := Options{
			Bundle: "1.1", Servers: []string{"10.0.9.9"}, HATier: "single-server",
			ControlPlaneInstall: true, Name: "prod-1", Domain: "example.test", AdminEmail: "admin@example.test",
			BackupTarget: "s3://kubenest-kit/" + recoveryTestScope + "?endpoint=minio.example.test&region=main",
			Recovery: &RecoveryOptions{
				RestoreFrom: "latest", Kit: "s3", FleetKey: f.fleet.SecretKeyString(),
				OldHostFenced: true, Kind: recoverykit.KindControlPlane,
			},
		}
		t.Setenv("KUBENEST_CHECKPOINT_ACCESS_KEY_ID", "checkpoint-key")
		t.Setenv("KUBENEST_CHECKPOINT_SECRET_ACCESS_KEY", "checkpoint-secret")
		s, runner := recoverySession(t, opts)
		s.Out = &bytes.Buffer{}
		s.recoveryStore = f.bucket
		s.recoveryCheckpointStore = f.bucket
		s.recoveryTargetValue = testTarget(t)
		// The select stage runs on every attempt (it is AlwaysRun), so the
		// session state the chart stage needs is built exactly as a resume
		// builds it.
		backendLooks := 0
		runner.respond = func(command string) (sshx.Result, error) {
			switch {
			// The readiness probe: three Deployments, the Postgres
			// StatefulSet, the certificate's condition and the Gateway's. The
			// backend reports not-ready for its first two observations, which
			// is what a pod that is still starting looks like.
			case strings.Contains(command, "get deployment/"):
				if strings.Contains(command, "kubenest-cp-backend") {
					backendLooks++
					if backendLooks <= 2 {
						return sshx.Result{Stdout: `{"metadata":{"generation":1},"spec":{"replicas":1,"template":{"metadata":{"annotations":{"kubenest.io/install-revision":"` + revisionOf(runner) + `"}}}},"status":{"observedGeneration":1,"replicas":1,"updatedReplicas":1,"availableReplicas":0}}`}, nil
					}
				}
				return sshx.Result{Stdout: `{"metadata":{"generation":1},"spec":{"replicas":1,"template":{"metadata":{"annotations":{"kubenest.io/install-revision":"` + revisionOf(runner) + `"}}}},"status":{"observedGeneration":1,"replicas":1,"updatedReplicas":1,"availableReplicas":1}}`}, nil
			case strings.Contains(command, "get statefulset"):
				return sshx.Result{Stdout: `{"spec":{"replicas":1},"status":{"readyReplicas":1}}`}, nil
			case strings.Contains(command, "get certificate/"), strings.Contains(command, "get gateway/"):
				return sshx.Result{Stdout: `{"status":{"conditions":[{"type":"Ready","status":"True"},{"type":"Programmed","status":"True"}]}}`}, nil
			case strings.Contains(command, "get service kubenest-cp-backend"):
				return sshx.Result{Stdout: "10.43.105.14"}, nil
			case strings.Contains(command, "get secret kubenest-cp-install"):
				// A fresh host: this recovery writes that Secret itself.
				return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): secrets "kubenest-cp-install" not found`}, nil
			case strings.Contains(command, "get pods") && strings.Contains(command, "postgresql"):
				return sshx.Result{Stdout: "kubenest-cp-postgresql-0"}, nil
			case strings.Contains(command, "psql"):
				if psqlErr != "" {
					return sshx.Result{ExitCode: 1, Stderr: psqlErr}, nil
				}
				return sshx.Result{Stdout: dbRows}, nil
			}
			return sshx.Result{}, nil
		}
		if err := stageRecoverySelectControlPlane(context.Background(), s); err != nil {
			t.Fatalf("the select stage failed: %v", err)
		}
		return s, runner
	}
	backendProbes := func(runner *recordingRunner) int {
		n := 0
		for _, command := range runner.ran {
			if strings.Contains(command, "get deployment/kubenest-cp-backend") {
				n++
			}
		}
		return n
	}
	appliedValues := func(runner *recordingRunner) string {
		var sb strings.Builder
		for _, in := range runner.inputs {
			sb.Write(in)
		}
		return sb.String()
	}

	// A resume whose database already holds the checkpoint: the chart goes back
	// on with the backend RUNNING, and the stage WAITS for it to serve — the
	// Helm controller installs asynchronously, and the next stage logs in.
	s, runner := build(t, "3\n", "")
	if err := stageRecoveryControlPlane(context.Background(), s); err != nil {
		t.Fatalf("the chart stage failed on a resume: %v", err)
	}
	if probes := backendProbes(runner); probes < 3 {
		t.Fatalf("the backend was probed %d time(s): the stage applied the chart with the backend running and returned without waiting for it, so the next stage logs in to a pod that is still starting", probes)
	}
	resumed := appliedValues(runner)
	if !strings.Contains(resumed, "replicas: 1") {
		t.Fatalf("the chart was applied with the backend still held after the checkpoint was loaded, so every later stage has nothing to talk to:\n%s", tailOf(resumed, 400))
	}
	if strings.Contains(resumed, "replicas: 0") {
		t.Fatal("the applied chart holds the backend at zero replicas on a resume whose database is loaded")
	}
	// And the log line says what this stage actually did.
	transcript := logOf(t, s)
	if !strings.Contains(transcript, "RUNNING") || !strings.Contains(transcript, "is serving through") {
		t.Fatalf("the stage does not report that it started and waited for the backend:\n%s", transcript)
	}
	if strings.Contains(transcript, "held at zero replicas") {
		t.Fatalf("the stage reports holding the backend on a path that started it:\n%s", transcript)
	}

	// A first attempt (no pod yet, nothing loaded): the chart goes on HELD,
	// which is what the load needs.
	first, firstRunner := build(t, "", `ERROR: relation "organization" does not exist`)
	if err := stageRecoveryControlPlane(context.Background(), first); err != nil {
		t.Fatalf("the chart stage failed on a first attempt: %v", err)
	}
	if held := appliedValues(firstRunner); !strings.Contains(held, "replicas: 0") {
		t.Fatalf("a first attempt applied the chart with the backend running, so it would build the schema before the checkpoint is loaded:\n%s", tailOf(held, 400))
	}
	if probes := backendProbes(firstRunner); probes != 0 {
		t.Fatalf("a first attempt probed the backend for readiness %d time(s): a held backend is not waited for", probes)
	}
	if transcript := logOf(t, first); !strings.Contains(transcript, "held at zero replicas") {
		t.Fatalf("a first attempt does not report holding the backend:\n%s", transcript)
	}
}

// logOf returns what the stage printed, so a claim about which path ran is
// read from the operator's own transcript rather than from a field.
func logOf(t *testing.T, s *Session) string {
	t.Helper()
	if buf, ok := s.Out.(*bytes.Buffer); ok {
		return buf.String()
	}
	t.Fatal("this session does not capture its output")
	return ""
}

// revisionPattern finds the install revision the stage rendered, in the
// document it applies.
var revisionPattern = regexp.MustCompile(`installRevision["\x27]?\s*:\s*["\x27]?([0-9a-f]{8,})`)

// revisionOf is the install revision the stage has applied so far, read from
// the document it sent: the readiness probe compares the Deployment's
// annotation with exactly this value, and the annotation is only in the
// document once the chart stage has applied it.
func revisionOf(runner *recordingRunner) string {
	out := ""
	for _, in := range runner.inputs {
		if match := revisionPattern.FindSubmatch(in); match != nil {
			out = string(match[1])
		}
	}
	return out
}

// tailOf is the last n characters, so a failure shows the end of an applied
// document rather than 200 KB of values.
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
