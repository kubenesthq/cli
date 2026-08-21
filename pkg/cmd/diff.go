package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"kubenest.io/cli/pkg/manifest"
)

// newPlatformDiffCommand shows what changes between two bundles, before
// anyone commits to finding out the hard way.
func newPlatformDiffCommand() *cobra.Command {
	var from, to string
	cmd := &cobra.Command{
		Use:     "diff",
		Short:   "Show what changes between two platform bundles",
		Example: `  kubenest platform diff --from 1.0 --to 1.1`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if from == "" || to == "" {
				return fmt.Errorf("--from and --to are both required")
			}
			ctx := cmd.Context()
			client, err := controlPlaneClient()
			if err != nil {
				return err
			}
			fromBundle, err := fetchManifest(ctx, client, from)
			if err != nil {
				return err
			}
			toBundle, err := fetchManifest(ctx, client, to)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), renderDiff(fromBundle, toBundle))
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "bundle version to compare from (required)")
	cmd.Flags().StringVar(&to, "to", "", "bundle version to compare to (required)")
	return cmd
}

// renderDiff reports every component whose pin moves, and says plainly which
// one is the point of no return — the Kubernetes version is the only change
// on the list that cannot be reverted in seconds.
func renderDiff(from, to *manifest.Manifest) string {
	keys := map[string]bool{}
	for k := range from.Core {
		keys[k] = true
	}
	for k := range to.Core {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)

	var b strings.Builder
	fmt.Fprintf(&b, "Bundle %s → %s\n\n", from.Bundle, to.Bundle)
	changed := 0
	for _, name := range names {
		was, now := from.Core[name], to.Core[name]
		if was == now {
			continue
		}
		changed++
		switch {
		case was == "":
			fmt.Fprintf(&b, "  + %-28s %s\n", name, now)
		case now == "":
			fmt.Fprintf(&b, "  - %-28s was %s\n", name, was)
		default:
			fmt.Fprintf(&b, "    %-28s %s → %s\n", name, was, now)
		}
	}
	if changed == 0 {
		b.WriteString("  no component versions change\n")
	}
	if from.Core["k3s"] != to.Core["k3s"] {
		fmt.Fprintf(&b, "\nKubernetes moves %s → %s. That is the one change here that cannot be reverted:\n",
			from.Core["k3s"], to.Core["k3s"])
		b.WriteString("every other component is a Helm release and rolls back in seconds, while going\n")
		b.WriteString("back from a Kubernetes version means restoring the datastore snapshot taken\n")
		b.WriteString("before the upgrade started.\n")
	}
	return b.String()
}
