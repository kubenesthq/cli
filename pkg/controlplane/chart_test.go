package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The version helm-controller reads from the archive is the version this
// package reports; there is no second source it could disagree with.
func TestChartVersionIsThePackagedChartVersion(t *testing.T) {
	version, err := ChartVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version != "3.0.0" {
		t.Errorf("ChartVersion = %q, want 3.0.0", version)
	}
	if len(ChartArchive()) == 0 {
		t.Fatal("ChartArchive is empty: the embedded chart did not make it into the binary")
	}
	zr, err := gzip.NewReader(bytes.NewReader(ChartArchive()))
	if err != nil {
		t.Fatalf("ChartArchive is not a gzip stream: %v", err)
	}
	defer zr.Close()
}

// DRIFT. kubenest-helm/kubenest owns the chart; chart/*.tgz here is a copy of
// what helm packaged from it, so that the binary carries the artifact it
// installs. If the two disagree, this CLI installs a chart nobody reviewed at
// the version it claims — so a chart edit that was not followed by
// re-packaging has to fail the build.
//
// Skips when the umbrella workspace is not checked out beside this repo
// (which is the case in this repo's own CI), because a check that cannot run
// must say so rather than pass.
func TestEmbeddedChartMatchesTheSiblingCheckout(t *testing.T) {
	root := filepath.Join("..", "..", "..", "kubenest-helm", "kubenest")
	if _, err := os.Stat(filepath.Join(root, "Chart.yaml")); err != nil {
		t.Skipf("kubenest-helm is not checked out beside this repo, so chart drift cannot be checked here: %v", err)
	}
	packaged := map[string][]byte{}
	for name, content := range archiveEntries(t) {
		relative, ok := strings.CutPrefix(name, chartDir+"/")
		if !ok {
			t.Errorf("the embedded archive carries %s, which is outside the %s/ chart directory", name, chartDir)
			continue
		}
		packaged[relative] = content
	}
	ignored := helmIgnore(t, filepath.Join(root, ".helmignore"))
	claimed := map[string]bool{}

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if ignored(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if whole, ok := packaged[rel]; ok {
			checkPackaged(t, rel, rel, body, whole)
			claimed[rel] = true
			return nil
		}
		// A vendored subchart travels either as its own tarball or — as helm 4
		// packages it — unpacked into the chart that vendors it. Both are
		// checked against the tarball on disk, which is what the chart's
		// maintainers edit and lock.
		if strings.HasSuffix(rel, ".tgz") {
			inside := tarballEntries(t, p)
			if len(inside) == 0 {
				t.Errorf("%s carries no files, so drift inside it cannot be checked", rel)
				return nil
			}
			for name, content := range inside {
				outer := path.Join(path.Dir(rel), name)
				got, ok := packaged[outer]
				if !ok {
					t.Errorf("%s holds %s, which the embedded archive does not carry: re-package the chart", rel, outer)
					continue
				}
				checkPackaged(t, name, name+" inside "+rel, content, got)
				claimed[outer] = true
			}
			return nil
		}
		t.Errorf("%s is in the chart directory but not in the embedded archive: re-package the chart into pkg/controlplane/chart/", rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// And nothing extra, in the other direction: a template deleted upstream
	// must stop being installed from this copy.
	for name := range packaged {
		if !claimed[name] {
			t.Errorf("%s is in the embedded archive but not in %s: re-package the chart", name, root)
		}
	}

	// Non-vacuity, and the files helm packages for every chart: the checks
	// above pass trivially if the walk or the test data collapses to nothing.
	for _, required := range []string{"Chart.yaml", "Chart.lock", "values.yaml"} {
		if !claimed[required] {
			t.Errorf("%s was not compared, so the check above proved nothing about it", required)
		}
	}
	templates, subcharts := 0, 0
	for name := range packaged {
		if strings.HasPrefix(name, "templates/") {
			templates++
		}
		if strings.HasSuffix(name, ".tgz") || (strings.HasPrefix(name, "charts/") && strings.HasSuffix(name, "/Chart.yaml")) {
			subcharts++
		}
	}
	if templates == 0 {
		t.Error("the embedded archive carries no templates/")
	}
	if subcharts < 2 {
		t.Errorf("the embedded archive carries %d vendored subcharts, want the chart's 2 (postgresql and redis)", subcharts)
	}

	// The version follows the archive too, so a version bump without a
	// re-package cannot be reported as installed.
	disk, err := os.ReadFile(filepath.Join(root, "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal(disk, &meta); err != nil {
		t.Fatal(err)
	}
	version, err := ChartVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version != meta.Version {
		t.Errorf("the embedded chart is version %q, but %s/Chart.yaml says %q: re-package the chart", version, root, meta.Version)
	}
}

// checkPackaged compares one chart-directory file with the entry helm
// packaged from it. source is the path whose base name decides how strict the
// comparison is; desc names both sides in a failure.
//
// Chart.yaml is the one file helm does not copy: packaging re-serializes it
// from the chart's metadata, so key order, quoting and line wrapping all
// change while the chart described stays the same chart. It is compared by
// value, which still catches a changed version, name or dependency.
func checkPackaged(t *testing.T, source, desc string, want, got []byte) {
	t.Helper()
	if path.Base(source) == "Chart.yaml" {
		var wantMeta, gotMeta map[string]any
		if err := yaml.Unmarshal(want, &wantMeta); err != nil {
			t.Fatalf("%s is not valid YAML: %v", desc, err)
		}
		if err := yaml.Unmarshal(got, &gotMeta); err != nil {
			t.Fatalf("the archive's copy of %s is not valid YAML: %v", desc, err)
		}
		if !reflect.DeepEqual(wantMeta, gotMeta) {
			t.Errorf("the archive's copy of %s says something different from the chart directory: re-package the chart", desc)
		}
		return
	}
	if !bytes.Equal(want, got) {
		t.Errorf("the archive's copy of %s is not byte-identical to the chart directory: re-package the chart", desc)
	}
}

// archiveEntries returns every regular file of the embedded archive, keyed by
// its path inside the archive.
func archiveEntries(t *testing.T) map[string][]byte {
	t.Helper()
	return tarballReader(t, gzipReader(t, bytes.NewReader(ChartArchive())))
}

// tarballEntries returns the regular files of one .tgz on disk.
func tarballEntries(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	defer f.Close()
	return tarballReader(t, gzipReader(t, f))
}

func gzipReader(t *testing.T, r io.Reader) *gzip.Reader {
	t.Helper()
	zr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("reading a gzip stream: %v", err)
	}
	return zr
}

func tarballReader(t *testing.T, zr *gzip.Reader) map[string][]byte {
	t.Helper()
	defer zr.Close()
	out := map[string][]byte{}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading a chart archive: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = body
	}
	return out
}

// helmIgnore compiles the chart's own .helmignore into a matcher over
// chart-relative slash paths, so the drift check compares exactly the files
// helm would package. The rules helm documents and this chart uses: blank
// lines and # comments are skipped, a pattern with a leading slash matches
// from the chart root, and a pattern without one matches a base name anywhere
// in the tree.
func helmIgnore(t *testing.T, ignoreFile string) func(rel string) bool {
	t.Helper()
	raw, err := os.ReadFile(ignoreFile)
	if err != nil {
		t.Fatalf("reading %s: %v", ignoreFile, err)
	}
	var patterns []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return func(rel string) bool {
		for _, pattern := range patterns {
			if anchored, ok := strings.CutPrefix(pattern, "/"); ok {
				if matched, _ := path.Match(anchored, rel); matched {
					return true
				}
				continue
			}
			if matched, _ := path.Match(pattern, path.Base(rel)); matched {
				return true
			}
		}
		return false
	}
}

// ---------------------------------------------------------------------------
// What the control-plane chart must say (kn-t47).
//
// These read the chart SOURCE (kubenest-helm/kubenest), like the drift test
// above, and assert on the rendered STRUCTURE — the YAML objects the templates
// describe — rather than on the template text, so a change that keeps the
// words and drops the object fails here.
//
// The templates are Go templates, not YAML: helper actions become a quoted
// placeholder and whole-line control-flow actions are dropped before parsing.
// That is enough to read the objects, and it is why these tests skip when the
// sibling checkout is absent instead of guessing at an archive.
// ---------------------------------------------------------------------------

// siblingChartRoot is where kubenest-helm is checked out beside this repo.
func siblingChartRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..", "..", "kubenest-helm", "kubenest")
	if _, err := os.Stat(filepath.Join(root, "Chart.yaml")); err != nil {
		t.Skipf("kubenest-helm is not checked out beside this repo, so the chart's objects cannot be read here: %v", err)
	}
	return root
}

// chartObjects parses one template's documents. An action on a line of its own
// is control flow (an if/with guard) and carries no object, so it is dropped;
// an action inside a value becomes a placeholder string.
func chartObjects(t *testing.T, root, name string) []map[string]any {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "templates", name))
	if err != nil {
		t.Fatalf("reading templates/%s: %v", name, err)
	}
	var kept []string
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "{{") && strings.HasSuffix(trimmed, "}}") && strings.Count(trimmed, "{{") == 1 {
			continue
		}
		kept = append(kept, line)
	}
	// A bare scalar, not a quoted one: an action inside quotes (`image: "{{ ... }}"`)
	// would otherwise end as two adjacent scalars and the document would not parse.
	rendered := actionPattern.ReplaceAllString(strings.Join(kept, "\n"), "__RENDERED__")

	var objects []map[string]any
	decoder := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("templates/%s does not parse once its template actions are placeholders: %v", name, err)
		}
		if len(doc) > 0 {
			objects = append(objects, doc)
		}
	}
	if len(objects) == 0 {
		t.Fatalf("templates/%s described no objects at all, so nothing below was checked", name)
	}
	return objects
}

var actionPattern = regexp.MustCompile(`(?s)\{\{-?.*?-?\}\}`)

func objectOfKind(t *testing.T, objects []map[string]any, kind string) map[string]any {
	t.Helper()
	for _, object := range objects {
		if object["kind"] == kind {
			return object
		}
	}
	t.Fatalf("no %s in the template", kind)
	return nil
}

// dig walks a parsed object to a nested key, failing the test when it is absent.
func dig(t *testing.T, object map[string]any, path ...string) any {
	t.Helper()
	var current any = object
	for _, key := range path {
		mapping, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("%s is not a mapping on the way to %v", key, path)
		}
		current, ok = mapping[key]
		if !ok {
			t.Fatalf("%v is missing on the way to %v", key, path)
		}
	}
	return current
}

func containerNamed(t *testing.T, podSpec map[string]any, key, name string) map[string]any {
	t.Helper()
	list, ok := podSpec[key].([]any)
	if !ok {
		return nil
	}
	for _, entry := range list {
		container, ok := entry.(map[string]any)
		if ok && container["name"] == name {
			return container
		}
	}
	return nil
}

func envOf(t *testing.T, container map[string]any, name string) map[string]any {
	t.Helper()
	list, _ := container["env"].([]any)
	for _, entry := range list {
		variable, ok := entry.(map[string]any)
		if ok && variable["name"] == name {
			return variable
		}
	}
	return nil
}

// A migration is a step, not a side effect of a pod starting, and two versions
// must never serve at once.
func TestBackendDeploymentHasNoMigrateInitContainerAndUsesRecreate(t *testing.T) {
	objects := chartObjects(t, siblingChartRoot(t), "backend-deployment.yaml")
	deployment := objectOfKind(t, objects, "Deployment")

	if strategy := dig(t, deployment, "spec", "strategy", "type"); strategy != "Recreate" {
		t.Errorf("spec.strategy.type = %v, want Recreate: the schema is migrated by a Job, so an old pod must never serve beside a migrated database", strategy)
	}

	podSpec := dig(t, deployment, "spec", "template", "spec").(map[string]any)
	if container := containerNamed(t, podSpec, "initContainers", "migrate"); container != nil {
		t.Errorf("the backend still runs the migrate initContainer: %v", container)
	}
	// And nothing else sneaks `alembic upgrade head` into the pod either.
	for _, key := range []string{"initContainers", "containers"} {
		list, _ := podSpec[key].([]any)
		for _, entry := range list {
			container, _ := entry.(map[string]any)
			if command, ok := container["command"].([]any); ok {
				for _, part := range command {
					if text, ok := part.(string); ok && strings.Contains(text, "alembic") {
						t.Errorf("container %v runs %q: the migration is a recorded Job, not a container", container["name"], text)
					}
				}
			}
		}
	}

	// The pod carries the agent key, and it is a different Secret key from the
	// one SECRET_KEY comes from.
	backend := containerNamed(t, podSpec, "containers", "backend")
	if backend == nil {
		t.Fatal("no backend container")
	}
	agent := envOf(t, backend, "AGENT_JWT_SECRET")
	if agent == nil {
		t.Fatal("the backend container has no AGENT_JWT_SECRET env")
	}
	ref := dig(t, agent, "valueFrom", "secretKeyRef").(map[string]any)
	if ref["key"] != "agent-jwt-secret" {
		t.Errorf("AGENT_JWT_SECRET comes from Secret key %v, want agent-jwt-secret", ref["key"])
	}
}

// A migration is a step the CLI runs, not something a pod does on the way up:
// the image's entrypoint migrates when MIGRATE_ON_START is not "false", so both
// the backend Deployment and the migration Job have to turn it off (kn-t47).
// Without it on the backend, every pod start migrated the database — including
// a freshly restored checkpoint's, before pg_restore had loaded it — and the
// schema check that is meant to refuse a mismatched start could never fire.
func TestNeitherTheBackendNorTheMigrationJobMigratesOnStart(t *testing.T) {
	root := siblingChartRoot(t)

	deployment := objectOfKind(t, chartObjects(t, root, "backend-deployment.yaml"), "Deployment")
	podSpec := dig(t, deployment, "spec", "template", "spec").(map[string]any)
	backend := containerNamed(t, podSpec, "containers", "backend")
	if backend == nil {
		t.Fatal("no backend container")
	}
	env := envOf(t, backend, "MIGRATE_ON_START")
	if env == nil {
		t.Fatal("the backend container does not set MIGRATE_ON_START: the image's entrypoint would migrate the database on every pod start")
	}
	if env["value"] != "false" {
		t.Errorf("the backend's MIGRATE_ON_START = %v, want \"false\"", env["value"])
	}

	job := objectOfKind(t, chartObjects(t, root, "migration-job.yaml"), "Job")
	jobSpec := dig(t, job, "spec", "template", "spec").(map[string]any)
	migrate := containerNamed(t, jobSpec, "containers", "migrate")
	if migrate == nil {
		t.Fatal("no migrate container")
	}
	command, _ := migrate["command"].([]any)
	if len(command) != 3 || command[0] != "alembic" || command[2] != "head" {
		t.Fatalf("the migrate container's command = %v, want alembic upgrade head", command)
	}
	env = envOf(t, migrate, "MIGRATE_ON_START")
	if env == nil || env["value"] != "false" {
		t.Errorf("the Job's MIGRATE_ON_START = %v, want \"false\": its entrypoint would migrate once and its command would migrate again", env)
	}
}

func TestHubReadsAgentJWTSecretAndNoJWTSecret(t *testing.T) {
	objects := chartObjects(t, siblingChartRoot(t), "hub-deployment.yaml")
	deployment := objectOfKind(t, objects, "Deployment")

	podSpec := dig(t, deployment, "spec", "template", "spec").(map[string]any)
	hub := containerNamed(t, podSpec, "containers", "hub")
	if hub == nil {
		t.Fatal("no hub container")
	}

	if stale := envOf(t, hub, "JWT_SECRET"); stale != nil {
		t.Errorf("the hub still reads JWT_SECRET (%v): it must not be given the key that signs user sessions", stale)
	}
	agent := envOf(t, hub, "AGENT_JWT_SECRET")
	if agent == nil {
		t.Fatal("the hub has no AGENT_JWT_SECRET env, so it cannot verify an agent token")
	}
	ref := dig(t, agent, "valueFrom", "secretKeyRef").(map[string]any)
	if ref["key"] != "agent-jwt-secret" {
		t.Errorf("the hub reads AGENT_JWT_SECRET from Secret key %v, want agent-jwt-secret", ref["key"])
	}
}

// The control plane's certificate is issued by its OWN CA, and the CA, the
// Issuer and the Certificate name the same objects.
func TestGatewayCertificateUsesTheControlPlaneIssuer(t *testing.T) {
	root := siblingChartRoot(t)

	certificate := objectOfKind(t, chartObjects(t, root, "gateway.yaml"), "Certificate")
	issuerRef := dig(t, certificate, "spec", "issuerRef").(map[string]any)
	if issuerRef["kind"] != "Issuer" {
		t.Errorf("the Certificate is issued by a %v: a ClusterIssuer is the host cluster's identity, and a control plane restored onto another cluster would be distrusted", issuerRef["kind"])
	}
	// A template action renders as a placeholder; what matters is that the
	// issuer is named by the chart and that the Issuer object exists under the
	// same name. Both come from the same helper-free expression, so compare the
	// rendered name through the objects themselves.
	name, _ := issuerRef["name"].(string)
	if name == "" {
		t.Fatal("the Certificate names no issuer")
	}

	objects := chartObjects(t, root, "issuer.yaml")
	secret := objectOfKind(t, objects, "Secret")
	issuer := objectOfKind(t, objects, "Issuer")

	if secret["type"] != "kubernetes.io/tls" {
		t.Errorf("the CA Secret is of type %v, want kubernetes.io/tls", secret["type"])
	}
	caSecretName := dig(t, issuer, "spec", "ca", "secretName")
	if caSecretName != secret["metadata"].(map[string]any)["name"] {
		t.Errorf("the Issuer signs from Secret %v but the chart creates %v", caSecretName, secret["metadata"].(map[string]any)["name"])
	}
	if issuer["metadata"].(map[string]any)["name"] != name {
		t.Errorf("the Certificate names issuer %v but the chart creates %v", name, issuer["metadata"].(map[string]any)["name"])
	}
	// The CA's key is part of the recovery set: a CA without it cannot renew
	// the control plane's certificate after a restore.
	data := dig(t, secret, "stringData").(map[string]any)
	for _, key := range []string{"tls.crt", "tls.key"} {
		if _, ok := data[key]; !ok {
			t.Errorf("the CA Secret carries no %s", key)
		}
	}
}
