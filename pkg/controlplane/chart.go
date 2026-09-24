// Package controlplane installs the KubeNest control plane — backend, hub,
// UI, PostgreSQL and Redis — into a cluster the bundle has already prepared.
//
// WHY THE CHART IS EMBEDDED, and not fetched. `kubenest platform install
// --control-plane` has to install the control plane on a cluster that has no
// control plane to ask, and nothing is hosted by us (decision 2026-08-26), so
// the chart has to travel with the binary that installs it. That is the same
// reason pkg/bundles embeds the bundle manifests: the pinned artifact is in
// the installer, so a standalone install and a registered install of the same
// CLI release cannot install different things.
//
// The archive is packaged from kubenest-helm/kubenest by
//
//	helm package <workspace>/kubenest-helm/kubenest -d <this package>/chart/
//
// and is installed through a k3s HelmChart custom resource's spec.chartContent
// (pkg/k3s): the archive travels inside the resource, so no chart repository,
// no registry and no network exist anywhere in this path. controlplane_test.go
// fails the build when the sibling chart checkout has been edited without
// re-packaging.
package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	_ "embed"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// The archive is embedded as a single file, deliberately. Go requires a
// //go:embed pattern naming a []byte to match EXACTLY ONE file, so a second
// archive left behind by a re-package (chart/kubenest-3.0.1.tgz beside
// chart/kubenest-3.0.0.tgz) fails the build with "invalid go:embed: multiple
// files for type []byte" rather than shipping a binary that installs
// whichever one the pattern happened to pick. A runtime count would be dead
// code in every binary that builds, so the requirement is stated where it can
// be enforced.
//
//go:embed chart/*.tgz
var chartArchive []byte

// chartDir is the chart's directory inside the archive. helm names the
// top-level entry after the chart, so this is the chart name as well.
const chartDir = "kubenest"

// archiveChartYAML is the chart metadata entry inside the archive.
const archiveChartYAML = chartDir + "/Chart.yaml"

// ChartArchive is the embedded chart archive, byte-for-byte as helm packaged
// it. Install base64-encodes it into the HelmChart's spec.chartContent.
func ChartArchive() []byte {
	return chartArchive
}

// ChartVersion is the version field of the embedded Chart.yaml — the version
// helm-controller reads from the archive itself, so this is the only version
// this package ever reports.
func ChartVersion() (string, error) {
	raw, err := archiveFile(archiveChartYAML)
	if err != nil {
		return "", err
	}
	var meta struct {
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal(raw, &meta); err != nil {
		return "", fmt.Errorf("the chart archive built into this CLI has an unparsable Chart.yaml: %w", err)
	}
	if meta.Version == "" {
		return "", fmt.Errorf("the chart archive built into this CLI declares no version in %s", archiveChartYAML)
	}
	return meta.Version, nil
}

// archiveFile returns one entry of the embedded chart archive. The name is
// the full path inside the archive, e.g. "kubenest/Chart.yaml".
func archiveFile(name string) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(chartArchive))
	if err != nil {
		return nil, fmt.Errorf("the chart archive built into this CLI is not a gzip stream: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("the chart archive built into this CLI has no %s entry", name)
		}
		if err != nil {
			return nil, fmt.Errorf("the chart archive built into this CLI is unreadable: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Name != name {
			continue
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("the %s entry of the embedded chart archive is unreadable: %w", name, err)
		}
		return content, nil
	}
}
