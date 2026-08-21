package upgrade

import (
	"testing"

	"kubenest.io/cli/pkg/manifest"
)

func parseManifest(t *testing.T, doc string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return m
}
