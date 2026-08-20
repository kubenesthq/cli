// Package cmd wires the kubenest command tree.
//
// The CLI is the platform installer: login, platform install/uninstall/upgrade
// and backup operations, per docs.kubenest.io/platform. The pre-2026 app-layer
// commands (apps, deploy, exec, logs, registry, teams) were removed: they are
// frozen scope under decision D1 and target a control-plane API surface
// (teams, X-Team-UUID) that no longer exists. They live on in git history.
package cmd

import (
	"github.com/spf13/cobra"

	"kubenest.io/cli/pkg/version"
)

// NewRootCommand builds the full kubenest command tree.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "kubenest",
		Short: "Install and operate the KubeNest Platform",
		Long: `kubenest installs and operates the KubeNest Platform: a pinned, tested
bundle of everything a Kubernetes cluster needs above the control plane,
installed onto Ubuntu hosts you supply over SSH.

Your SSH keys and cloud credentials stay on this machine. They are never
uploaded to the control plane and never written to logs.`,
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	root.AddCommand(
		NewLoginCommand(),
		NewLogoutCommand(),
		NewPlatformCommand(),
		NewBackupCommand(),
	)
	return root
}
