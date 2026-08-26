package install_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/storage"
)

// standaloneSession is an install with no control plane, which is the whole
// point: API is nil and every branch under test derives from that.
//
// The runner is pkg/component/componenttest's scripted transport, the same
// one every component package's unit tests use. It exercises what the
// installer SAYS to a host, not what a host does with it — the real cluster
// behaviour is the e2e suite's and kn-sf17's to prove, and nothing here
// claims otherwise.
func standaloneSession(t *testing.T, runner *componenttest.FakeRunner) *install.Session {
	t.Helper()
	m, err := manifest.Parse([]byte("bundle: \"1.0\"\nlimits:\n  timeouts:\n    install-total: 30m\n    component-ready: 10m\n"))
	if err != nil {
		t.Fatal(err)
	}
	opts := install.Options{
		Bundle: "1.0", Name: "prod-1", Servers: []string{"10.0.1.10"},
		HATier: "single-server", Profiles: []string{},
	}
	j, err := install.OpenJournal(filepath.Join(t.TempDir(), "journal.json"), opts.Identity())
	if err != nil {
		t.Fatal(err)
	}
	s := &install.Session{
		ID: "run-1", Opts: opts, Bundle: m, Jnl: j,
		Emit: install.NopEmitter{}, Out: io.Discard,
	}
	if runner != nil {
		s.Nodes = []install.Node{{Address: "10.0.1.10", Role: install.RoleServer, Runner: runner}}
	}
	return s
}

func stageNamed(t *testing.T, s *install.Session, name string) install.Stage {
	t.Helper()
	for _, st := range install.Plan(s) {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("no stage named %q in the plan", name)
	return install.Stage{}
}

func TestStandaloneIsExactlyTheAbsenceOfAControlPlane(t *testing.T) {
	s := standaloneSession(t, nil)
	if !s.Standalone() {
		t.Fatal("a session with no API client is a standalone install")
	}
}

// Stage 2 with nothing to register against generates this cluster's own
// identity, records that it did, and mints NOTHING. The absence of
// credentials is the assertion that matters: a placeholder credential here
// would install an agent whose identity nothing recognises.
func TestStandaloneRegisterGeneratesAnIdentityAndMintsNothing(t *testing.T) {
	s := standaloneSession(t, nil)

	if err := stageNamed(t, s, install.StageRegister).Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if s.Jnl.ClusterID == "" {
		t.Fatal("standalone register produced no cluster identity")
	}
	if len(s.Jnl.ClusterID) != 36 || strings.Count(s.Jnl.ClusterID, "-") != 4 {
		t.Errorf("cluster identity %q is not the UUID shape the operator's cluster-id field carries", s.Jnl.ClusterID)
	}
	if s.Creds != nil {
		t.Errorf("standalone register minted credentials: %#v — with no hub there is no second party to authenticate to, so there must be no bearer", s.Creds)
	}
	if !s.Record.Standalone {
		t.Error("the record does not say this install was standalone, so a later adoption cannot tell it apart from a journal belonging to another control plane")
	}

	recorded, err := install.Recorded(s.Jnl)
	if err != nil {
		t.Fatal(err)
	}
	if !recorded.Standalone {
		t.Error("Record.Standalone did not survive the journal")
	}
}

// Two identities for one cluster would orphan whatever the first was written
// into. The registered path's mint cannot be idempotent; this one must be.
func TestStandaloneRegisterIsIdempotentAcrossAResume(t *testing.T) {
	s := standaloneSession(t, nil)
	stage := stageNamed(t, s, install.StageRegister)

	if err := stage.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := s.Jnl.ClusterID

	if err := stage.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Jnl.ClusterID != first {
		t.Errorf("a resumed standalone register regenerated the identity: %s then %s", first, s.Jnl.ClusterID)
	}
}

func TestEveryStandaloneIdentityIsDistinct(t *testing.T) {
	seen := map[string]bool{}
	for range 64 {
		s := standaloneSession(t, nil)
		if err := stageNamed(t, s, install.StageRegister).Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if seen[s.Jnl.ClusterID] {
			t.Fatalf("identity %s was generated twice", s.Jnl.ClusterID)
		}
		seen[s.Jnl.ClusterID] = true
	}
}

// Stage 10 must refuse rather than install an agent that can never converge,
// and the refusal must name the fix — which here is a bead, because the fix
// is not in this repo.
func TestStandaloneAgentStageRefusesAndNamesTheOperatorBead(t *testing.T) {
	s := standaloneSession(t, &componenttest.FakeRunner{})

	err := stageNamed(t, s, install.StageAgent).Run(context.Background())
	if err == nil {
		t.Fatal("stage 10 reported success on a cluster whose agent cannot reach Ready")
	}
	for _, want := range []string{"kn-sf17", "hub", "Ready"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	if got := install.ComponentOf(err); got != "kubenest-agent" {
		t.Errorf("the refusal is tagged %q, so a failure-injection run would not learn which component broke", got)
	}
}

// Stage 12 in standalone mode writes the record onto the cluster, because
// there is nowhere else for it to live and an unrecorded cluster cannot be
// safely upgraded.
func TestStandaloneRecordStageWritesTheRecordToTheCluster(t *testing.T) {
	runner := &componenttest.FakeRunner{}
	s := standaloneSession(t, runner)
	s.Record.Ownership = storage.InstallerCreated
	if err := stageNamed(t, s, install.StageRegister).Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := stageNamed(t, s, install.StageRecord).Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	commands := runner.Commands()
	if len(commands) == 0 {
		t.Fatal("stage 12 wrote nothing to the cluster")
	}
	applied := strings.Join(commands, "\n")
	if !strings.Contains(applied, "kubectl apply") {
		t.Errorf("stage 12 did not apply anything: %s", applied)
	}
	// The document goes over the wire base64-encoded to stay clear of
	// shell quoting, so assert on what would be decoded rather than on the
	// command text.
	doc, err := install.ClusterRecordManifest(install.ClusterRecord{
		ClusterID: s.Jnl.ClusterID, ClusterName: "prod-1", BundleVersion: "1.0",
		Profiles: []string{}, HATier: "single-server",
		VolumeGroupOwnership: string(storage.InstallerCreated), Standalone: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(applied, base64.StdEncoding.EncodeToString([]byte(doc))) {
		t.Errorf("stage 12 applied a document other than this cluster's record.\nwanted the encoding of:\n%s", doc)
	}
}

// The record is what uninstall reads to decide whether it may remove a volume
// group, and what an upgrade reads to know what it is starting from. Losing a
// field here is the difference between a clean teardown and destroying a
// customer's data.
func TestClusterRecordCarriesWhatUninstallAndUpgradeRead(t *testing.T) {
	want := install.ClusterRecord{
		ClusterID: "019d52e1-ba17-7e70-94a0-8a33a48b7fcb", ClusterName: "prod-1",
		BundleVersion: "1.0", Profiles: []string{"observability"}, HATier: "ha",
		VolumeGroupOwnership: string(storage.InstallerCreated), Standalone: true,
	}
	doc, err := install.ClusterRecordManifest(want)
	if err != nil {
		t.Fatal(err)
	}

	var cm struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal([]byte(doc), &cm); err != nil {
		t.Fatalf("the rendered record is not valid YAML: %v\n%s", err, doc)
	}
	if cm.Kind != "ConfigMap" || cm.Metadata.Name != install.ClusterRecordName || cm.Metadata.Namespace != install.ClusterRecordNamespace {
		t.Errorf("the record went somewhere unexpected: %s/%s %s", cm.Metadata.Namespace, cm.Metadata.Name, cm.Kind)
	}

	var got install.ClusterRecord
	if err := json.Unmarshal([]byte(cm.Data[install.ClusterRecordKey]), &got); err != nil {
		t.Fatalf("the record does not round-trip: %v\n%s", err, doc)
	}
	if got.ClusterID != want.ClusterID || got.ClusterName != want.ClusterName ||
		got.BundleVersion != want.BundleVersion || got.HATier != want.HATier ||
		got.VolumeGroupOwnership != want.VolumeGroupOwnership || !got.Standalone {
		t.Errorf("the record did not survive rendering:\n got %+v\nwant %+v", got, want)
	}
	if len(got.Profiles) != 1 || got.Profiles[0] != "observability" {
		t.Errorf("the profile set did not survive: %v", got.Profiles)
	}
}

// A cluster with no record has nothing an upgrade can start from, and saying
// so beats returning a zero value that reads as bundle "".
func TestReadingAnAbsentRecordSaysSoRatherThanReturningNothing(t *testing.T) {
	runner := &componenttest.FakeRunner{}
	_, err := install.ReadClusterRecord(context.Background(), runner)
	if err == nil {
		t.Fatal("an absent record was reported as a valid one")
	}
	if !strings.Contains(err.Error(), install.ClusterRecordName) {
		t.Errorf("the error does not name the object that is missing: %v", err)
	}
}

// Written and read back through the same names: a reader carrying its own
// copy of a writer's names is a reader that will one day report "never
// installed" about a cluster that was.
func TestTheRecordWrittenIsTheRecordRead(t *testing.T) {
	want := install.ClusterRecord{
		ClusterID: "cluster-1", ClusterName: "prod-1", BundleVersion: "1.0",
		Profiles: []string{}, HATier: "single-server",
		VolumeGroupOwnership: string(storage.CustomerCreated), Standalone: true,
	}
	doc, err := install.ClusterRecordManifest(want)
	if err != nil {
		t.Fatal(err)
	}
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal([]byte(doc), &cm); err != nil {
		t.Fatal(err)
	}
	body := cm.Data[install.ClusterRecordKey]

	runner := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		if !strings.Contains(command, install.ClusterRecordName) {
			t.Errorf("the reader looked for something other than the record: %s", command)
		}
		return sshx.Result{Stdout: "'" + body + "'"}, nil
	}}
	got, err := install.ReadClusterRecord(context.Background(), runner)
	if err != nil {
		t.Fatal(err)
	}
	if got.BundleVersion != want.BundleVersion || got.ClusterID != want.ClusterID || !got.Standalone {
		t.Errorf("round trip lost fields:\n got %+v\nwant %+v", got, want)
	}
}

// The catalog a standalone preflight checks against is the one built into
// this binary, and it must be non-empty — an empty catalog would refuse every
// install with a message about the CLI rather than about the request.
func TestEmbeddedCatalogOffersTheBundlesThisBinaryCarries(t *testing.T) {
	entries, err := install.EmbeddedCatalog{}.ListBundles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("a standalone install has no bundles to check the request against")
	}
	for _, e := range entries {
		if e.Version == "" || len(e.HATiers) == 0 {
			t.Errorf("catalog entry is unusable for preflight: %+v", e)
		}
	}
}
