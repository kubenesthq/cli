package converge_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/manifest"
)

// The bead's acceptance test, end to end: the deadline is read from the
// bundle manifest, and changing ONLY the manifest flips the same check on the
// same cluster behavior from fail to pass. No constant in the code has a say.
func TestChangingTheManifestChangesTheVerdict(t *testing.T) {
	writeBundle := func(componentReady string) string {
		doc := "bundle: \"1.0\"\nlimits:\n  timeouts:\n    component-ready: " + componentReady + "\n"
		path := filepath.Join(t.TempDir(), "bundle.yaml")
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// The same "cluster": a component that needs ~80ms to come up.
	newProbe := func() converge.Probe {
		start := time.Now()
		return func(ctx context.Context) (bool, converge.State, error) {
			if time.Since(start) < 80*time.Millisecond {
				return false, converge.State{Object: "deploy cert-manager", Status: "0/1 Ready"}, nil
			}
			return true, converge.State{Object: "deploy cert-manager", Status: "1/1 Ready"}, nil
		}
	}

	run := func(bundlePath string) converge.Outcome {
		t.Helper()
		m, err := manifest.Load(bundlePath)
		if err != nil {
			t.Fatal(err)
		}
		deadline, err := m.Limits.Timeouts.For("component-ready")
		if err != nil {
			t.Fatal(err)
		}
		res, err := converge.Wait(context.Background(), newProbe(), converge.Options{
			Name: "cert-manager-ready", Deadline: deadline, Interval: 5 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		return res.Outcome
	}

	if got := run(writeBundle("20ms")); got != converge.Fail {
		t.Errorf("with a 20ms component-ready in the manifest the check = %s, want fail", got)
	}
	if got := run(writeBundle("2s")); got != converge.Pass {
		t.Errorf("with a 2s component-ready in the manifest the same check = %s, want pass", got)
	}
}
