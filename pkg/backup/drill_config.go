package backup

import (
	"context"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
)

// DrillConfigName is the in-cluster handoff from the bundle-aware CLI to the
// always-on operator. It lives beside Velero so configuring a target works
// before the later kubenest-agent install stage creates kubenest-system.
const DrillConfigName = "kubenest-restore-drill"

// EnsureDrillConfiguration records the manifest-owned cadence and deadline
// for the operator's scheduled runner. It contains no credentials; target
// credentials remain in their purpose-specific Secrets.
func EnsureDrillConfiguration(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest) error {
	drill, err := bundle.Backup.Defaults.Drill()
	if err != nil {
		return err
	}
	deadline, err := bundle.Limits.Timeouts.For("restore-drill")
	if err != nil {
		return err
	}

	doc, err := yaml.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      DrillConfigName,
			"namespace": Namespace,
		},
		"data": map[string]string{
			"interval": drill.Interval.Duration().String(),
			"timeout":  deadline.String(),
		},
	})
	if err != nil {
		return err
	}
	return apply(ctx, r, "restore drill configuration", doc)
}
