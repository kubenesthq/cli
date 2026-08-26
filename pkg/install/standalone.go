package install

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/bundles"
	"kubenest.io/cli/pkg/component/traefik"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/preflight"
	"kubenest.io/cli/pkg/storage"
)

// Standalone mode: `kubenest platform install` with no control plane at all.
//
// DECIDED 2026-08-26 (kn-l827): nothing is hosted by us. There is no
// multi-tenant KubeNest environment and none is a prerequisite for anything,
// so a cluster has to be installable, and complete, on its own. The control
// plane becomes the FLEET product — what a customer adds when they have more
// than one cluster and want the console and the multi-cluster day-2 view —
// and a cluster that never gets one is not a degraded install.
//
// What that changes, and what it deliberately does not:
//
//   - Stage 1 checks the request against the bundle catalog built into this
//     binary instead of the control plane's. The CHECK is the same; only the
//     source of the offer moves.
//   - Stage 2 mints nothing. It generates this cluster's own id and records
//     it. See localClusterID for why no token is generated with it.
//   - Stage 12 writes the bundle record onto the cluster, because in this
//     mode there is nowhere else for it to live and an unrecorded cluster
//     cannot be safely upgraded.
//   - Everything from stage 3 to stage 9 is byte-for-byte the same work. A
//     standalone cluster is not a different platform.

// Standalone reports whether this install has no control plane.
//
// It is derived from the session rather than stored, so there is exactly one
// answer and no way for a flag and a client to disagree about it.
func (s *Session) Standalone() bool { return s.API == nil }

// EmbeddedCatalog is preflight's view of the bundles built into this binary.
// It is what stage 1 checks the request against when there is no control
// plane to ask.
type EmbeddedCatalog struct{}

// ListBundles returns the offered bundles from the embedded catalog.
func (EmbeddedCatalog) ListBundles(context.Context) ([]preflight.BundleEntry, error) {
	entries, err := bundles.Catalog()
	if err != nil {
		return nil, err
	}
	out := make([]preflight.BundleEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, preflight.BundleEntry{Version: e.Version, HATiers: e.HATiers, Profiles: e.Profiles})
	}
	return out, nil
}

// localClusterID generates this cluster's own identity.
//
// IT IS AN IDENTITY, NOT A CREDENTIAL, AND NOTHING IS MINTED ALONGSIDE IT.
// In a registered install the agent JWT exists to authenticate the cluster to
// the hub. With no hub there is no second party to authenticate to, so there
// is no bearer to hold — and generating a self-signed one locally to satisfy
// a validator would be a credential trusted by nothing, existing only to get
// past a check. That is the kind of thing that reads as real six months
// later. If there is no bearer, there must be no bearer (kn-sf17 carries the
// operator-side change that makes the agent accept an identity without one).
//
// The shape is a UUID because that is what the operator's cluster-id field
// and every existing cluster record already carry; the value is random, from
// crypto/rand, and unique to this cluster.
func localClusterID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating this cluster's identity: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// stageRegisterStandalone is stage 2 with nothing to register against.
//
// Idempotent across a resume, which the registered path's mint cannot be: an
// identity already in the journal is reused rather than regenerated, because
// generating a second one would orphan whatever the first was written into.
func stageRegisterStandalone(s *Session) error {
	if s.Jnl.ClusterID == "" {
		id, err := localClusterID()
		if err != nil {
			return err
		}
		s.Jnl.ClusterID = id
		s.Logf("  standalone: cluster identity %s generated locally; nothing was registered anywhere", id)
	} else {
		s.Logf("  standalone: reusing cluster identity %s from this journal", s.Jnl.ClusterID)
	}
	s.Record.Standalone = true
	// No credentials: stage 10 is told what it is missing and why, rather
	// than being handed a placeholder that would install an agent whose
	// identity nothing recognises.
	s.Creds = nil
	return s.saveRecord()
}

// ClusterRecordName is the ConfigMap stage 12 writes the bundle record into
// on a standalone cluster.
//
// It lives ON THE CLUSTER because in this mode there is nowhere else. That is
// also the right shape rather than a fallback: pkg/upgrade's InClusterDrills
// already reads the restore-drill evidence from the cluster on the stated
// reasoning that "the CLI and the control plane are looking at one source of
// truth rather than two representations of it". kn-y3gt is the reader — an
// upgrade that starts from this record instead of a remote one.
const ClusterRecordName = "kubenest-cluster-record"

// ClusterRecordKey is the ConfigMap key the record document lives under.
const ClusterRecordKey = "record.json"

// ClusterRecord is what a standalone cluster knows about itself.
//
// Every field is what an upgrade needs to know what it is starting from, and
// none of them is a secret — the same rule the install journal's Record
// follows, for the same reason: this object is readable by anything with get
// on the namespace.
type ClusterRecord struct {
	ClusterID     string   `json:"cluster_id"`
	ClusterName   string   `json:"cluster_name"`
	BundleVersion string   `json:"bundle_version"`
	Profiles      []string `json:"profiles"`
	HATier        string   `json:"ha_tier"`
	// VolumeGroupOwnership is what uninstall reads to decide whether it may
	// ever remove a volume group. A wrong value here is the difference
	// between a clean teardown and destroying a customer's data.
	VolumeGroupOwnership string `json:"volume_group_ownership"`
	// Standalone records that no control plane minted this cluster's
	// identity, so a later adoption can tell this apart from a journal that
	// belongs to some other control plane.
	Standalone bool `json:"standalone"`
}

// stageRecordStandalone is stage 12 with no control plane to record against.
func stageRecordStandalone(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	ownership := s.Record.Ownership
	if ownership == "" {
		ownership = storage.CustomerCreated
	}
	profiles := s.Opts.Profiles
	if profiles == nil {
		profiles = []string{}
	}
	record := ClusterRecord{
		ClusterID:            s.Jnl.ClusterID,
		ClusterName:          s.Opts.Name,
		BundleVersion:        s.Opts.Bundle,
		Profiles:             profiles,
		HATier:               s.Opts.HATier,
		VolumeGroupOwnership: string(ownership),
		Standalone:           true,
	}
	doc, err := ClusterRecordManifest(record)
	if err != nil {
		return err
	}
	if err := kubectlApply(ctx, server, doc); err != nil {
		return fmt.Errorf("writing this cluster's bundle record: %w", err)
	}
	s.Logf("  standalone: recorded bundle %s on the cluster (configmap %s/%s)",
		s.Opts.Bundle, ClusterRecordNamespace, ClusterRecordName)
	return nil
}

// ClusterRecordNamespace is the platform namespace. It comes from the package
// that creates it (stage 5 installs Traefik there) rather than a literal
// here, so a platform that moves namespace cannot leave the record being
// written into one nothing else uses.
const ClusterRecordNamespace = traefik.Namespace

// ClusterRecordManifest renders the record's ConfigMap. Exported so a test can
// read exactly what would be applied without a cluster.
func ClusterRecordManifest(record ClusterRecord) (string, error) {
	body, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	doc, err := yaml.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      ClusterRecordName,
			"namespace": ClusterRecordNamespace,
			"labels": map[string]string{
				"app.kubernetes.io/managed-by": "kubenest-cli",
			},
		},
		"data": map[string]string{ClusterRecordKey: string(body)},
	})
	if err != nil {
		return "", err
	}
	return string(doc), nil
}

// ReadClusterRecord reads a standalone cluster's own record back off it.
//
// Absent is not an error the caller can be spared: a cluster with no record
// has nothing an upgrade can start from, and saying so is more useful than
// returning a zero value that reads as "bundle """.
func ReadClusterRecord(ctx context.Context, r k3s.Runner) (ClusterRecord, error) {
	out, err := k3s.Kubectl(ctx, r, fmt.Sprintf(
		"get configmap %s -n %s -o jsonpath='{.data.%s}' --ignore-not-found",
		ClusterRecordName, ClusterRecordNamespace, escapeJSONPathKey(ClusterRecordKey)))
	if err != nil {
		return ClusterRecord{}, err
	}
	body := trimQuotes(out)
	if body == "" {
		return ClusterRecord{}, fmt.Errorf(
			"this cluster carries no bundle record (configmap %s/%s), so there is nothing to say what is installed on it: "+
				"a cluster installed by this CLI writes one at its record stage",
			ClusterRecordNamespace, ClusterRecordName)
	}
	var record ClusterRecord
	if err := json.Unmarshal([]byte(body), &record); err != nil {
		return ClusterRecord{}, fmt.Errorf("this cluster's bundle record could not be read: %w", err)
	}
	if record.BundleVersion == "" {
		return ClusterRecord{}, fmt.Errorf("this cluster's bundle record names no bundle version")
	}
	return record, nil
}

// escapeJSONPathKey escapes the dots in a ConfigMap key so kubectl's jsonpath
// reads one key called "record.json" rather than a nested path.
func escapeJSONPathKey(key string) string {
	return strings.ReplaceAll(key, ".", `\.`)
}

// trimQuotes strips the surrounding quotes kubectl's jsonpath output carries
// and the whitespace around them.
func trimQuotes(out string) string {
	return strings.Trim(strings.TrimSpace(out), "'")
}
