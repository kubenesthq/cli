package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/agent"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/sshx"
)

// rotateFlags is the whole surface. The node flags exist for the same reason
// upgrade's do: the install journal on this machine usually knows the servers,
// and an operator running from a different machine has to name them.
type rotateFlags struct {
	Cluster string
	Servers []string
	SSHUser string
	SSHKey  string
}

// howToFinish is the sentence every failure after the rotation must end with.
// It is a constant because the recovery is always the same command: the
// rotation is done, the delivery is not, and re-running rotates again and
// delivers again. There is no partial state that needs a different repair.
const howToFinish = "the cluster is DISCONNECTED and its new token is not delivered. " +
	"Re-run `kubenest cluster rotate-token --cluster %s` to rotate again and finish delivery"

// runRotate rotates a cluster's agent token and delivers it, and does not
// return success until the cluster is connected again.
//
// THE ORDER IS THE WHOLE POINT AND THE FIRST STEP IS IRREVERSIBLE. Once the
// control plane has raised the revocation floor the cluster is offline, so
// every failure from that moment must say so. Reporting an error without
// saying the cluster is down is the outcome this command exists to prevent.
func runRotate(ctx context.Context, out io.Writer, f rotateFlags) error {
	client, err := controlPlaneClient()
	if err != nil {
		return err
	}
	clusterID, err := resolveCluster(ctx, client, f.Cluster)
	if err != nil {
		return err
	}

	// Everything that can fail WITHOUT taking the cluster offline is done
	// before the rotation, so an unreachable node or a missing journal is an
	// ordinary error rather than an outage.
	servers := f.Servers
	if len(servers) == 0 {
		if path, pathErr := install.JournalPath(f.Cluster); pathErr == nil {
			if journal, readErr := install.ReadJournal(path); readErr == nil {
				servers, _ = install.NodesFromJournal(journal)
			}
		}
	}
	if len(servers) == 0 {
		return fmt.Errorf("no server node for cluster %q: this machine has no install journal for it, so pass --server for the host that carries the agent manifest. NOTHING HAS BEEN ROTATED", f.Cluster)
	}
	recorded, err := client.BundleRecord(ctx, clusterID)
	if err != nil {
		return fmt.Errorf("reading the cluster's bundle record: %w. NOTHING HAS BEEN ROTATED", err)
	}
	bundle, err := fetchManifest(ctx, client, recorded.BundleVersion)
	if err != nil {
		return fmt.Errorf("reading bundle %s: %w. NOTHING HAS BEEN ROTATED", recorded.BundleVersion, err)
	}
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return fmt.Errorf("%w. NOTHING HAS BEEN ROTATED", err)
	}

	sshOpts := sshx.Options{User: f.SSHUser, KeyPath: f.SSHKey}
	endpoint, err := sshx.Resolve(servers[0], sshOpts)
	if err != nil {
		return fmt.Errorf("%s: %w. NOTHING HAS BEEN ROTATED", servers[0], err)
	}
	node, err := sshx.Dial(ctx, endpoint, sshOpts)
	if err != nil {
		return fmt.Errorf("%s: %w. NOTHING HAS BEEN ROTATED", servers[0], err)
	}
	defer func() { _ = node.Close() }()

	// ---- from here the cluster goes offline ----
	fmt.Fprintf(out, "Rotating the agent token for %s. The cluster goes offline until the new token is delivered.\n", f.Cluster)
	rotation, err := client.RotateAgentToken(ctx, clusterID)
	if err != nil {
		return err
	}
	if rotation.Enforcement == "pending" {
		// Not an error: the floor is committed and every snapshot carries it.
		// Worth saying, because "in force within one snapshot" is different
		// from "in force now" if someone is rotating a leaked token.
		fmt.Fprintf(out, "  the hub did not confirm the new revocation floor in time; it is committed and takes effect within one snapshot interval\n")
	}

	fmt.Fprintf(out, "Delivering the new token to %s.\n", servers[0])
	if err := agent.DeliverJWT(ctx, node, rotation.AgentJWT.Token.Reveal(), deadline, nil); err != nil {
		return fmt.Errorf("%w\n"+howToFinish, err, f.Cluster)
	}

	fmt.Fprintf(out, "Waiting for %s to reconnect.\n", f.Cluster)
	if err := waitConnected(ctx, client, clusterID, deadline); err != nil {
		return fmt.Errorf("%w\n"+howToFinish, err, f.Cluster)
	}
	fmt.Fprintf(out, "Rotated. %s is connected again on token version %d.\n", f.Cluster, rotation.AgentJWT.TokenVersion)
	return nil
}

// waitConnected polls until the control plane reports the cluster connected.
//
// THE OPERATOR DEPLOYMENT BECOMING AVAILABLE IS NOT ENOUGH. It proves the node
// accepted the manifest, not that the token in it authenticates: a rejected
// token leaves a running pod that cannot reach the hub. Only the control
// plane's own view of the connection distinguishes them, which is why this
// asks the control plane rather than the node.
func waitConnected(ctx context.Context, client *api.Client, clusterID string, deadline time.Duration) error {
	const interval = 5 * time.Second
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	var last string
	for {
		cluster, err := client.Cluster(ctx, clusterID)
		if err == nil {
			if cluster.Status == "connected" {
				return nil
			}
			last = cluster.Status
		} else {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the cluster did not reconnect within %s (last seen: %s)", deadline, last)
		case <-time.After(interval):
		}
	}
}
