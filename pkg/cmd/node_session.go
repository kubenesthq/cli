package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/node"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/preflight"
	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/upgrade"
)

// nodeRecords is the cluster's record as a node verb uses it: read it, write
// it back with the revision that was read.
//
// It is deliberately NOT upgrade.ControlPlaneRecords: that store refuses a
// record with no bundle version by telling the operator there is "nothing to
// upgrade FROM", which is an upgrade's question. A node verb reads the same
// record and answers its own question (the k3s version a node must join at).
type nodeRecords struct {
	client    *api.Client
	clusterID string
}

func (r nodeRecords) Load(ctx context.Context) (api.ClusterBundle, error) {
	record, err := r.client.BundleRecord(ctx, r.clusterID)
	if err != nil {
		return api.ClusterBundle{}, fmt.Errorf("reading the cluster's record: %w", err)
	}
	return record, nil
}

func (r nodeRecords) Save(ctx context.Context, record api.BundleRecord) error {
	return r.client.PutBundleRecord(ctx, r.clusterID, record)
}

// nodeCatalog is the control plane as preflight needs it: the offer of bundle
// versions a request is checked against. *api.Client's own list carries
// pkg/api's types, so the two shapes are joined here rather than by widening
// either.
type nodeCatalog struct{ client *api.Client }

func (c nodeCatalog) ListBundles(ctx context.Context) ([]preflight.BundleEntry, error) {
	if c.client == nil {
		return nil, fmt.Errorf("no control plane configured: run `kubenest login` first")
	}
	entries, err := c.client.ListBundles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]preflight.BundleEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, preflight.BundleEntry{Version: e.Version, HATiers: e.HATiers, Profiles: e.Profiles})
	}
	return out, nil
}

// prepareNodeSession resolves everything a node verb needs before its stages
// run: the cluster's id, its record, the manifest of the bundle it records,
// its maintenance window, and the operation's journal.
//
// The journal's IDENTITY carries what makes a resume the SAME operation — the
// verb, the cluster, the machine, the storage flag and the bundle — because a
// resume into a half-finished cluster with changed arguments is how a cluster
// ends up not matching its own record.
func prepareNodeSession(ctx context.Context, out io.Writer, client *api.Client, cluster, kind, machine, storageDevice, sshUser, sshKey string) (*node.Session, error) {
	clusterID, err := resolveCluster(ctx, client, cluster)
	if err != nil {
		return nil, err
	}
	records := nodeRecords{client: client, clusterID: clusterID}
	recorded, err := records.Load(ctx)
	if err != nil {
		return nil, err
	}
	if recorded.BundleVersion == "" {
		return nil, fmt.Errorf("this cluster's record names no bundle version, so the versions a node must match are unknown. Re-run the install's record stage")
	}
	bundle, err := fetchManifest(ctx, client, recorded.BundleVersion)
	if err != nil {
		return nil, err
	}
	// The window comes from upgrade's reader: a window that could not be read
	// is kept as an ERROR rather than as "no window", which is what makes the
	// difference between "any time will do" and "nobody can tell".
	window, windowErr := upgrade.ControlPlaneRecords{Client: client, ClusterID: clusterID}.Window(ctx)

	identity := stages.Identity{
		Kind:    kind,
		Cluster: cluster,
		Fields: map[string]string{
			"bundle":  recorded.BundleVersion,
			"machine": machine,
			"storage": storageDevice,
		},
	}
	journalPath, err := stages.JournalPath(kind, cluster)
	if err != nil {
		return nil, err
	}
	journal, err := stages.OpenJournal(journalPath, identity)
	if err != nil {
		return nil, err
	}

	return &node.Session{
		ID:        stages.NewRunID(),
		Cluster:   cluster,
		ClusterID: clusterID,
		Records:   records,
		Bundle:    bundle,
		Catalog:   nodeCatalog{client: client},
		Egress:    install.EgressTargets(&install.Session{Bundle: bundle}),
		Jnl:       journal,
		Emit:      stages.Emitters{stages.TextEmitter{W: out}},
		Out:       out,
		Window:    window,
		WindowErr: windowErr,
		Now:       func() time.Time { return time.Now().UTC() },
		Dial: func(ctx context.Context, host api.HostRecord) (node.Transport, error) {
			return node.DialHost(ctx, host, sshUser, sshKey)
		},
		Store: func(runner k3s.Runner) *operation.Store {
			return &operation.Store{
				Runner:          runner,
				Operator:        upgrade.OperatorName(),
				Mirror:          client,
				MirrorClusterID: clusterID,
			}
		},
		Metadata: nodeMetadata{cluster: cluster, bundle: recorded.BundleVersion},
	}, nil
}
