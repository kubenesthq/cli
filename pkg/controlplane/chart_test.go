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
