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
	// THE BASELINE FOR STEP 3, READ BEFORE THE ROTATION BECAUSE AFTERWARDS IT IS
	// GONE (kn-tlmv). Waiting for Status to read "connected" cannot prove a
	// reconnection: nothing moves that column when the rotation drops the live
	// connection, so it still holds the value the PRE-ROTATION heartbeat left
	// there and a wait on it returns instantly. What moves only when the NEW
	// token authenticates is last_heartbeat, and proving movement needs the
	// earlier value in hand. Reading it here also keeps it on the right side of
	// the line: a failure now is an ordinary error, not an outage.
	baselineCluster, err := client.Cluster(ctx, clusterID)
	if err != nil {
		return fmt.Errorf("reading the cluster's current state: %w. NOTHING HAS BEEN ROTATED", err)
	}
	baseline := takenBaseline(baselineCluster.LastHeartbeat)

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
	if err := waitConnected(ctx, client, clusterID, baseline, deadline); err != nil {
		return fmt.Errorf("%w\n"+howToFinish, err, f.Cluster)
	}
	fmt.Fprintf(out, "Rotated. %s is connected again on token version %d.\n", f.Cluster, rotation.AgentJWT.TokenVersion)
	return nil
}

// waitConnected polls until the control plane reports the cluster connected
// ON A HEARTBEAT LATER THAN THE ONE IT HAD BEFORE THE ROTATION.
//
// THE OPERATOR DEPLOYMENT BECOMING AVAILABLE IS NOT ENOUGH. It proves the node
// accepted the manifest, not that the token in it authenticates: a rejected
// token leaves a running pod that cannot reach the hub. Only the control
// plane's own view of the connection distinguishes them, which is why this
// asks the control plane rather than the node.
//
// AND THE STATUS COLUMN ALONE IS NOT ENOUGH EITHER, WHICH IS WHY `since` IS
// REQUIRED RATHER THAN OPTIONAL (kn-tlmv). Exactly one thing in the backend
// moves Cluster.status away from "connected" — the health sweeper — and it does
// so from a CLOCK, on a silence window sized from the cluster's own declared
// cadence. Its code says why that is right: "a transport close is not a cluster
// being down". Rotation drops the hub connection and writes nothing to the row.
// So a wait keyed on the value alone reads the PRE-ROTATION heartbeat's
// "connected", returns on its first poll, and reports success over a cluster
// that may never come back. That is the single outcome this command exists to
// prevent, so the test is a TRANSITION: a heartbeat strictly later than the one
// we held before. The old token is below the revocation floor by now and cannot
// authenticate, so that timestamp cannot advance until the NEW token does —
// which makes its movement proof rather than a proxy.
//
// BOTH TIMESTAMPS COME FROM THE CONTROL PLANE, deliberately. Comparing against
// the local clock would import the skew between this machine and the server for
// no gain.
//
// `since` is nil for a cluster that has never reported. Then any timestamp at
// all is an advance, which is the correct reading rather than a special case.
func waitConnected(ctx context.Context, client *api.Client, clusterID string, since heartbeatBaseline, deadline time.Duration) error {
	// A BASELINE THAT WAS NEVER TAKEN IS REFUSED HERE RATHER THAN TREATED AS
	// "never reported", and that distinction is the whole reason this is a type
	// instead of a *time.Time (kn-6b22, kn-k9ko's precedent).
	//
	// MEASURED, not reasoned: with a bare pointer, deleting the pre-rotation
	// read at the call site made `since` nil, nil compared as "any heartbeat is
	// an advance", the pre-fix defect was back IN FULL, and all five unit tests
	// for this function still passed — because they pass `since` explicitly and
	// so pin the function while leaving the call site free. Refusing an untaken
	// baseline turns that silent reintroduction into a loud one.
	if !since.taken {
		return fmt.Errorf("internal: no pre-rotation heartbeat baseline was taken, so a reconnection cannot be proven; refusing to report success. NOTHING about the cluster's state is being claimed here")
	}
	const interval = 5 * time.Second
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	var last string
	for {
		cluster, err := client.Cluster(ctx, clusterID)
		if err == nil {
			if cluster.Status == "connected" && heartbeatAdvanced(since.at, cluster.LastHeartbeat) {
				return nil
			}
			last = describeWait(cluster, since.at)
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

// heartbeatBaseline is the cluster's last_heartbeat AS READ BEFORE THE
// ROTATION, carrying the fact that it was read at all.
//
// The zero value means NO BASELINE WAS TAKEN, which is not the same as "this
// cluster has never reported" — the first is a bug in the sequencer, the second
// is an ordinary state of a fresh cluster. A bare *time.Time collapses them,
// and collapsing them is what let the defect come back unnoticed.
type heartbeatBaseline struct {
	taken bool
	at    *time.Time
}

// takenBaseline records a baseline that was actually read. It is the only way
// to produce a heartbeatBaseline that waitConnected will accept, so a call site
// cannot forget the read and still be believed.
func takenBaseline(at *time.Time) heartbeatBaseline {
	return heartbeatBaseline{taken: true, at: at}
}

// heartbeatAdvanced reports whether `now` is a report strictly later than
// `since`. A missing `now` is never an advance: absence of a timestamp is not
// evidence of a heartbeat.
func heartbeatAdvanced(since, now *time.Time) bool {
	if now == nil {
		return false
	}
	if since == nil {
		return true
	}
	return now.After(*since)
}

// describeWait says which of the two conditions is still outstanding, because
// "not connected yet" and "connected on a stale heartbeat" need different
// things from whoever is reading the failure.
func describeWait(cluster api.Cluster, since *time.Time) string {
	if cluster.Status != "connected" {
		return cluster.Status
	}
	if cluster.LastHeartbeat == nil {
		return "connected, but the control plane has recorded no heartbeat"
	}
	return fmt.Sprintf("connected, but on the same heartbeat as before the rotation (%s) — the new token has not been accepted",
		cluster.LastHeartbeat.UTC().Format(time.RFC3339))
}
