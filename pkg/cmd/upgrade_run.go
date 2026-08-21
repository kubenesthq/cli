package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/upgrade"
	"kubenest.io/cli/pkg/window"
)

// UpgradeFlags is the flag surface of `kubenest platform upgrade`.
type UpgradeFlags struct {
	Cluster string
	To      string
	SSHUser string
	SSHKey  string
	// Acknowledge accepts individual deprecation findings by
	// namespace/Kind/name. There is deliberately no blanket --force.
	Acknowledge []string
	// Servers and Agents override the node list when there is no local
	// install journal — an upgrade run from a different machine than the
	// install.
	Servers []string
	Agents  []string
}

// buildUpgradeSession assembles everything an upgrade needs: the cluster's
// own record, both bundle manifests, the node connections, the maintenance
// window, and the journal.
func buildUpgradeSession(ctx context.Context, out io.Writer, f UpgradeFlags) (*upgrade.Session, error) {
	client, err := controlPlaneClient()
	if err != nil {
		return nil, err
	}

	clusterID, err := resolveCluster(ctx, client, f.Cluster)
	if err != nil {
		return nil, err
	}
	// The cluster's own record is the authority on what it IS. An upgrade
	// that took its starting point from an argument could move a cluster it
	// had misidentified.
	recorded, err := upgrade.LoadRecord(ctx, client, clusterID)
	if err != nil {
		return nil, err
	}

	from, err := fetchManifest(ctx, client, recorded.BundleVersion)
	if err != nil {
		return nil, err
	}
	to, err := fetchManifest(ctx, client, f.To)
	if err != nil {
		return nil, err
	}

	servers, agents := f.Servers, f.Agents
	if len(servers) == 0 {
		// The install journal on this machine knows the nodes. Without it,
		// the operator names them.
		if path, pathErr := install.JournalPath(f.Cluster); pathErr == nil {
			if journal, readErr := install.ReadJournal(path); readErr == nil {
				servers, agents = install.NodesFromJournal(journal)
			}
		}
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("no nodes for cluster %q: this machine has no install journal for it, so pass --server (and --agent) for the hosts to upgrade", f.Cluster)
	}

	opts := upgrade.Options{
		Cluster: f.Cluster, To: f.To,
		Servers: servers, Agents: agents,
		SSHUser: f.SSHUser, SSHKey: f.SSHKey,
		Acknowledge: f.Acknowledge,
	}

	journalPath, err := upgrade.JournalPath(f.Cluster)
	if err != nil {
		return nil, err
	}
	journal, err := stages.OpenJournal(journalPath, opts.Identity(recorded.BundleVersion))
	if err != nil {
		return nil, err
	}
	journal.ClusterID = clusterID

	session := &upgrade.Session{
		ID:       stages.NewRunID(),
		Opts:     opts,
		From:     from,
		To:       to,
		Jnl:      journal,
		Reporter: converge.NewTextReporter(out),
		Out:      out,
		API:      client,
		Cluster:  recorded,
	}
	session.Emit = stages.Emitters{
		stages.TextEmitter{W: out},
		stages.NewControlPlaneEmitter(client, func() string { return journal.ClusterID }),
	}
	if err := session.Connect(ctx); err != nil {
		return nil, err
	}
	return session, nil
}

// resolveCluster turns a cluster name into its control-plane id.
func resolveCluster(ctx context.Context, client *api.Client, name string) (string, error) {
	orgs, err := client.ListOrgs(ctx)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, org := range orgs {
		clusters, err := client.ListOrgClusters(ctx, org.ID)
		if err != nil {
			return "", err
		}
		for _, c := range clusters {
			if c.Name == name {
				matches = append(matches, c.ID)
			}
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no cluster named %q is registered with this control plane", name)
	default:
		return "", fmt.Errorf("more than one cluster is named %q, which should be impossible: names are unique across KubeNest", name)
	}
}

func fetchManifest(ctx context.Context, client *api.Client, version string) (*manifest.Manifest, error) {
	raw, err := client.BundleManifest(ctx, version)
	if err != nil {
		return nil, err
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bundle %s from the control plane is not a valid manifest: %w", version, err)
	}
	return m, nil
}

// runUpgrade is `kubenest platform upgrade`.
func runUpgrade(ctx context.Context, out io.Writer, f UpgradeFlags) error {
	session, err := buildUpgradeSession(ctx, out, f)
	if err != nil {
		return err
	}
	defer session.Close()

	fmt.Fprintf(out, "Upgrading %s from bundle %s to %s.\n", f.Cluster, session.From.Bundle, session.To.Bundle)
	fmt.Fprintf(out, "Components first, Kubernetes last: everything before the kubernetes stage reverts\nin seconds. That stage is the point of no return.\n\n")

	result, err := stages.Execute(ctx, session, upgrade.Plan(session))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\nUpgraded to %s in %s.\n", session.To.Bundle, result.Elapsed.Round(time.Second))
	if len(result.Skipped) > 0 {
		fmt.Fprintf(out, "Skipped %d stage(s) completed by an earlier run: %s\n",
			len(result.Skipped), strings.Join(result.Skipped, ", "))
	}
	return nil
}

// runRollback is `kubenest platform rollback`.
//
// It reports which mechanism it will use BEFORE doing anything, and asks for
// confirmation when that mechanism is a datastore restore — which is a
// service interruption, not a revert.
func runRollback(ctx context.Context, out io.Writer, in io.Reader, f UpgradeFlags, confirmed bool) error {
	session, err := buildUpgradeSession(ctx, out, f)
	if err != nil {
		return err
	}
	defer session.Close()

	plan := session.RollbackPlan()
	fmt.Fprint(out, plan)

	switch plan.Mechanism {
	case upgrade.MechanismNothing:
		return nil
	case upgrade.MechanismRestore:
		if !confirmed {
			return fmt.Errorf("this rollback restores the datastore snapshot, which interrupts service: pass --confirm to proceed")
		}
	}
	return session.Rollback(ctx, plan)
}

// runWindow is `kubenest cluster set-window`.
func runWindow(ctx context.Context, out io.Writer, cluster string, spec window.Spec) error {
	parsed, err := window.Parse(spec)
	if err != nil {
		return err
	}
	client, err := controlPlaneClient()
	if err != nil {
		return err
	}
	clusterID, err := resolveCluster(ctx, client, cluster)
	if err != nil {
		return err
	}
	if err := client.PutMaintenanceWindow(ctx, clusterID, api.MaintenanceWindow{
		Days: spec.Days, Start: spec.Start, End: spec.End, Timezone: spec.Timezone,
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "Maintenance window for %s is now %s.\n", cluster, parsed)
	fmt.Fprintf(out, "Upgrades will not START outside it, and OS reboots are held for it.\n")
	return nil
}
