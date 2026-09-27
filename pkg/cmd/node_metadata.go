package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/s3"
)

// nodeMetadata refreshes the cluster's recovery metadata after a node
// operation (PLAN 7.1: "each completed node operation also refreshes the
// cluster's recovery metadata in S3, so recovery never depends on an
// install-time copy").
//
// WHAT IT REFRESHES is the cluster's recovery SET — the manifest a recovery
// selects a kit and a backup by — and it updates the two things a node
// operation can make stale: the bundle version the recovery will need, and
// when the set was last written. The SET is written by the install's kit stage
// and by `kubenest backup now` (pkg/cmd/backup.go's recordRecoverySet); this is
// the third writer of the same document, and it reuses pkg/recoverykit's own
// Set type rather than reimplementing what a set is.
//
// IT FAILS LOUDLY AND HARMLESSLY. A laptop with no local kit (a node added
// from a second laptop, which is the supported case) or no bucket credentials
// cannot write it; that is reported, and the operation record keeps the write
// OWED rather than letting a completed node operation read as if nothing were
// outstanding.
type nodeMetadata struct {
	// cluster is the cluster's NAME, which is what names its install journal.
	cluster string
	// bundle is the bundle the cluster records now: the version a recovery
	// starting from this set would need.
	bundle string
}

func (m nodeMetadata) Refresh(ctx context.Context, cluster string) (string, error) {
	// The local install journal names the cluster's immutable id, and the
	// local kit copy names its artifact and its bucket. Without them there is
	// nothing here to write a set from.
	journalPath, err := install.JournalPath(cluster)
	if err != nil {
		return "", err
	}
	journal, err := install.ReadJournal(journalPath)
	if err != nil || journal == nil || journal.ClusterID == "" {
		return "", fmt.Errorf("no install journal for %q on this machine, so the recovery set's cluster id is unknown: run `kubenest recovery-kit check` from wherever the journal is", cluster)
	}
	kit, err := recoverykit.NewestLocal(journal.ClusterID, recoverykit.KindCluster)
	if err != nil {
		return "", err
	}
	accessKeyID := envFirst("KUBENEST_BACKUP_ACCESS_KEY_ID", "AWS_ACCESS_KEY_ID")
	secretAccessKey := envFirst("KUBENEST_BACKUP_SECRET_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY")
	if accessKeyID == "" || secretAccessKey == "" {
		return "", fmt.Errorf("no bucket credentials in the environment (KUBENEST_BACKUP_ACCESS_KEY_ID / KUBENEST_BACKUP_SECRET_ACCESS_KEY), so the recovery set could not be read or written")
	}
	client, err := s3.New(s3.Config{
		Endpoint:        kit.S3Location.Endpoint,
		Bucket:          kit.S3Location.Bucket,
		Region:          kit.S3Location.Region,
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
	})
	if err != nil {
		return "", err
	}
	scope := strings.Trim(kit.S3Location.Prefix, "/")
	key := recoverykit.SetKey(scope, journal.ClusterID, recoverykit.KindCluster, kit.ArtifactID)

	raw, err := client.Get(ctx, key)
	if errors.Is(err, s3.ErrNotFound) {
		return "", fmt.Errorf("no recovery set at %s: the install writes one before anything may depend on it, so a missing one means recovery cannot select this cluster's kit. `kubenest recovery-kit check` reports what is there", key)
	}
	if err != nil {
		return "", fmt.Errorf("reading the recovery set at %s: %w", key, err)
	}
	set, err := recoverykit.LoadSet(raw)
	if err != nil {
		return "", err
	}
	// The set must belong to THIS cluster and this kit: refreshing another
	// cluster's set (a wrong prefix, a copied bucket) would be rewriting a
	// document that describes different machines.
	if err := set.Verify(kit.Binding); err != nil {
		return "", err
	}
	if set.Versions == nil {
		set.Versions = map[string]string{}
	}
	before := set.Versions["bundle"]
	set.Versions["bundle"] = m.bundle
	set.WrittenAt = time.Now().UTC()
	body, err := set.Document()
	if err != nil {
		return "", err
	}
	if err := client.Put(ctx, key, body); err != nil {
		return "", fmt.Errorf("writing the recovery set at %s: %w", key, err)
	}
	if before == "" {
		return fmt.Sprintf("the recovery set %s now names bundle %s, so a recovery selects the versions this cluster runs", key, m.bundle), nil
	}
	return fmt.Sprintf("the recovery set %s now names bundle %s (it named %s), so a recovery selects the versions this cluster runs", key, m.bundle, before), nil
}
