package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/k3s"
)

// The expected-coverage record (plan 7.5, kn-t210).
//
// A backup that "Completed" with one volume that got no copy at all is the
// support incident this record exists to prevent: read against Velero's own
// copy results alone, such a backup looks complete, `kubenest backup restore`
// picks it, and the customer discovers the loss during the restore. So the set
// of namespaces and claims the backup is EXPECTED to cover is enumerated once,
// when the backup starts, with their UIDs, and every later judgement is made
// against that set — a claim in it with no copy is missing, not "nothing to
// copy".
//
// One record, one shape, two writers: this CLI writes it for backups
// `kubenest backup now` creates, the operator writes it for the ones Velero's
// schedule creates. First writer wins, so the set is never re-enumerated. The
// canonical shape is pinned in
// kubenest-contracts/testdata/backup-coverage-record.json, and the operator
// reads the same JSON with its own struct because the two modules cannot share
// one.
const (
	// CoverageRecordPrefix names one backup's record:
	// kubenest-coverage-<backup-name>, in the velero namespace, the same
	// namespace and evidence style as the restore drill's result.
	CoverageRecordPrefix = "kubenest-coverage-"
	coverageRecordKey    = "coverage.json"
	coverageRecordedBy   = "cli"
	// coverageLabelKey marks the ConfigMap as KubeNest's own coverage record
	// rather than a ConfigMap that merely has the name. Both writers set it.
	coverageLabelKey = "kubenest.io/backup-coverage"
	coverageLabel    = "record"
	// coverageManagedBy is the value both writers use for
	// app.kubernetes.io/managed-by. It is the product, not the process: the
	// record is written by whichever of the CLI and the operator got there
	// first, and the reader must accept both.
	coverageManagedBy = "kubenest"
)

// CoverageRecordName is the one name a backup's coverage record can have.
func CoverageRecordName(backup string) string { return CoverageRecordPrefix + backup }

// coverageRecord is written as coverage.json. CaptureStartedAt is a pointer so
// the record can say null explicitly: a record written before Velero has set
// the Backup's status.startTimestamp does not know when capture started, and
// the reader prefers the Backup's own value anyway.
type coverageRecord struct {
	Backup           string              `json:"backup"`
	RecordedAt       string              `json:"recorded_at"`
	RecordedBy       string              `json:"recorded_by"`
	CaptureStartedAt *string             `json:"capture_started_at"`
	Namespaces       []coverageNamespace `json:"namespaces"`
}

type coverageNamespace struct {
	Name    string           `json:"name"`
	UID     string           `json:"uid"`
	Volumes []coverageVolume `json:"volumes"`
}

type coverageVolume struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// namespaceInventory is the slice of `kubectl get namespaces -o json` the
// record needs. The UID, not the name: a namespace deleted and recreated under
// the same name is a different namespace, and the record has to be able to say
// that the copy belongs to the one it claimed.
type namespaceInventory struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"metadata"`
	} `json:"items"`
}

// claimInventory is the slice of `kubectl get persistentvolumeclaims
// --all-namespaces -o json` the record needs, including creationTimestamp,
// which is how a claim created after the capture started is kept out of the
// expected set.
type claimInventory struct {
	Items []struct {
		Metadata struct {
			Namespace         string `json:"namespace"`
			Name              string `json:"name"`
			UID               string `json:"uid"`
			CreationTimestamp string `json:"creationTimestamp"`
		} `json:"metadata"`
	} `json:"items"`
}

// recordExpectedCoverage writes the expected set for a backup this CLI just
// created. It runs BEFORE the settle wait, so the set is recorded when the
// backup starts rather than after it ends — an enumeration taken later could
// add a claim the backup never claimed, or drop one it did.
//
// The ConfigMap is created, never applied: `create` fails on the second writer,
// which is what makes the record immutable in the ordinary case and what lets
// both writers race safely. A failure here is an error rather than silence: the
// one chance to record the set is now, and the operator will not re-enumerate
// it later without the same guarantee.
func recordExpectedCoverage(ctx context.Context, r k3s.Runner, name string) error {
	out, err := k3s.Kubectl(ctx, r, "get backup "+name+" -n "+Namespace+" -o json")
	if err != nil {
		return fmt.Errorf("read the backup that was just created: %w", err)
	}
	var created struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
		Status struct {
			StartTimestamp string `json:"startTimestamp"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		return fmt.Errorf("parsing `kubectl get backup %s -o json`: %w", name, err)
	}
	if created.Metadata.UID == "" {
		// Without the UID the record cannot be owned by the Backup, so Velero's
		// TTL would leave it behind for every backup ever taken.
		return fmt.Errorf("backup %s in %s has no UID, so its expected coverage cannot be recorded", name, Namespace)
	}

	namespaces, err := expectedCoverage(ctx, r, created.Status.StartTimestamp)
	if err != nil {
		return err
	}
	var captureStartedAt *string
	if created.Status.StartTimestamp != "" {
		captureStartedAt = &created.Status.StartTimestamp
	}
	payload, err := json.Marshal(coverageRecord{
		Backup:           name,
		RecordedAt:       time.Now().UTC().Format(time.RFC3339),
		RecordedBy:       coverageRecordedBy,
		CaptureStartedAt: captureStartedAt,
		Namespaces:       namespaces,
	})
	if err != nil {
		return fmt.Errorf("encode the expected coverage record for %s: %w", name, err)
	}

	doc, err := yaml.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      CoverageRecordName(name),
			"namespace": Namespace,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": coverageManagedBy,
				coverageLabelKey:               coverageLabel,
			},
			// Velero owns the Backup and expires it on its TTL; the owner
			// reference is what takes the record with it.
			"ownerReferences": []map[string]any{{
				"apiVersion": "velero.io/v1",
				"kind":       "Backup",
				"name":       name,
				"uid":        created.Metadata.UID,
			}},
		},
		"data": map[string]string{coverageRecordKey: string(payload)},
	})
	if err != nil {
		return err
	}
	if err := create(ctx, r, "coverage record for backup "+name, doc); err != nil {
		if strings.Contains(err.Error(), "AlreadyExists") {
			// First writer wins. A record that already exists was enumerated
			// when this backup started and must not be replaced by a later one.
			return nil
		}
		return err
	}
	return nil
}

// expectedCoverage enumerates the namespaces and claims a backup is expected to
// cover. A claim created after captureStartedAt is left out: the backup never
// claimed it, so its absence from the copies is not a failure.
func expectedCoverage(ctx context.Context, r k3s.Runner, captureStartedAt string) ([]coverageNamespace, error) {
	out, err := k3s.Kubectl(ctx, r, "get namespaces -o json")
	if err != nil {
		return nil, fmt.Errorf("list namespaces for the coverage record: %w", err)
	}
	var namespaces namespaceInventory
	if err := json.Unmarshal([]byte(out), &namespaces); err != nil {
		return nil, fmt.Errorf("parsing `kubectl get namespaces -o json`: %w", err)
	}
	out, err = k3s.Kubectl(ctx, r, "get persistentvolumeclaims --all-namespaces -o json")
	if err != nil {
		return nil, fmt.Errorf("list persistent volume claims for the coverage record: %w", err)
	}
	var claims claimInventory
	if err := json.Unmarshal([]byte(out), &claims); err != nil {
		return nil, fmt.Errorf("parsing `kubectl get persistentvolumeclaims -o json`: %w", err)
	}

	excluded := make(map[string]bool, len(workloadExcludedNamespaces))
	for _, name := range workloadExcludedNamespaces {
		excluded[name] = true
	}
	byNamespace := map[string][]coverageVolume{}
	for _, item := range claims.Items {
		claim := item.Metadata
		if excluded[claim.Namespace] {
			continue
		}
		switch {
		case claim.Namespace == "":
			return nil, fmt.Errorf("a persistent volume claim in the cluster listing has no namespace")
		case claim.Name == "":
			return nil, fmt.Errorf("a persistent volume claim in namespace %s has no name", claim.Namespace)
		case claim.UID == "":
			return nil, fmt.Errorf("persistent volume claim %s/%s has no UID, so the expected coverage set cannot be recorded",
				claim.Namespace, claim.Name)
		}
		if !claimedByBackup(claim.CreationTimestamp, captureStartedAt) {
			continue
		}
		byNamespace[claim.Namespace] = append(byNamespace[claim.Namespace], coverageVolume{
			Namespace: claim.Namespace,
			Name:      claim.Name,
			UID:       claim.UID,
		})
	}

	covered := make([]coverageNamespace, 0, len(namespaces.Items))
	for _, item := range namespaces.Items {
		namespace := item.Metadata
		if excluded[namespace.Name] {
			continue
		}
		if namespace.Name == "" {
			continue
		}
		if namespace.UID == "" {
			return nil, fmt.Errorf("namespace %s has no UID, so the expected coverage set cannot be recorded", namespace.Name)
		}
		volumes := byNamespace[namespace.Name]
		if volumes == nil {
			// An empty list, never null: the record says "this namespace was
			// expected and holds nothing", which is a fact, not a silence.
			volumes = []coverageVolume{}
		}
		sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
		covered = append(covered, coverageNamespace{Name: namespace.Name, UID: namespace.UID, Volumes: volumes})
	}
	sort.Slice(covered, func(i, j int) bool { return covered[i].Name < covered[j].Name })
	return covered, nil
}

// claimedByBackup reports whether a claim existed when the capture started. An
// unknown timestamp on either side is treated as "it did": dropping a volume
// that WAS expected would hide a missing copy, which is the failure this whole
// record exists to make visible.
func claimedByBackup(createdAt, captureStartedAt string) bool {
	if createdAt == "" || captureStartedAt == "" {
		return true
	}
	created, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return true
	}
	started, err := time.Parse(time.RFC3339, captureStartedAt)
	if err != nil {
		return true
	}
	return !created.After(started)
}

// create pipes one YAML document into `kubectl create -f -`, the same way and
// for the same reason as apply: the document travels over STDIN, never in the
// command string sshd makes readable to every user on the host. It exists
// beside apply because "create and fail if it is there" is the desired
// behaviour for evidence that must be written exactly once.
func create(ctx context.Context, r k3s.Runner, what string, doc []byte) error {
	res, err := r.RunInput(ctx, "sudo -n k3s kubectl create -f -", bytes.NewReader(doc))
	if err != nil {
		return fmt.Errorf("create %s: %w", what, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("create %s: exit %d: %s", what, res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}
