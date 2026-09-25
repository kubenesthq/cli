package recoverykit

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
)

// ControlPlaneScope is the bucket prefix the control plane's own principal
// writes under.
//
// It is a SEPARATE principal from every cluster's, and that separation is the
// tenant boundary this package exists to protect: a credential that can list
// or read this prefix has the control plane's recovery material and every
// other cluster's in reach. `kubenest backup set-target` therefore probes for
// it and refuses a credential that reaches it.
const ControlPlaneScope = "control-plane/"

// KitsDir and SetsDir are the directories inside one scope. The scope itself
// is a cluster's own prefix (the `--prefix` of its backup target, or
// ControlPlaneScope for the instance's own artifacts).
const (
	KitsDir = "recovery-kits"
	SetsDir = "recovery-sets"
)

// KitKey is where one sealed kit lives:
//
//	<scope>/recovery-kits/<cluster-id>/<kind>-<artifact-id>.json
//
// The kit is keyed by artifact id, so writing a new kit never overwrites the
// one an existing recovery set references.
func KitKey(scope, clusterID string, kind Kind, artifactID string) string {
	return path.Join(strings.Trim(scope, "/"), KitsDir, clusterID, string(kind)+"-"+artifactID+".json")
}

// SetKey is where one recovery set lives:
//
//	<scope>/recovery-sets/<cluster-id>/<kind>-<artifact-id>.json
//
// <cluster-id> is the cluster the set belongs to; for the instance's own
// control-plane kit it is the MANAGEMENT cluster — the one the control plane
// runs in — so the instance's artifacts sit beside that cluster's and the
// control plane's own separate copy lands at the same relative path under its
// own scope.
//
// One set per kit artifact: a set written with each backup updates the set for
// the kit that cluster currently uses, and a new kit gets a new object rather
// than redefining what an old one meant.
func SetKey(scope, clusterID string, kind Kind, artifactID string) string {
	return path.Join(strings.Trim(scope, "/"), SetsDir, clusterID, string(kind)+"-"+artifactID+".json")
}

// Backup is one backup a recovery set covers: what it is, when it completed,
// which namespaces it holds and how it ended. Only a Completed backup is
// eligible to be offered as the latest a recovery can start from.
type Backup struct {
	Name        string    `json:"name"`
	CompletedAt time.Time `json:"completed_at"`
	Coverage    []string  `json:"coverage,omitempty"`
	Status      string    `json:"status"`
}

// Completed reports whether this backup finished. A partial or failed upload
// is never eligible.
func (b Backup) Completed() bool { return b.Status == "Completed" }

// Set is a recovery set: the small plaintext manifest, in the bucket, that
// binds a kit to the backups it opens and to the exact versions they need.
//
// It carries NO key material — fingerprints only — so it can be read by anyone
// who can read the bucket, which is what makes it useful at recovery time,
// before the fleet key is anywhere near the machine.
type Set struct {
	Binding            Binding           `json:"binding"`
	ArtifactID         string            `json:"artifact_id"`
	Fingerprints       map[string]string `json:"fingerprints"`
	VeleroRepositoryID string            `json:"velero_repository_id,omitempty"`
	S3Location         Location          `json:"s3_location"`
	Backups            []Backup          `json:"backups,omitempty"`
	Versions           map[string]string `json:"versions,omitempty"`
	Checksums          map[string]string `json:"checksums,omitempty"`
	WrittenAt          time.Time         `json:"written_at"`
	// Complete is whether the upload that produced this set finished. A set
	// that is not complete is never offered as "latest".
	Complete bool `json:"complete"`
}

// NewSet binds a kit to what a recovery would start from.
//
// The set is built from the kit's own header, so a set can never describe a
// kit other than the one it points at: the artifact id, the fingerprints and
// the kit's digest all come from the kit, not from the caller's memory of it.
func NewSet(kit *Kit, versions map[string]string, kitDigest string, now time.Time) (*Set, error) {
	if kit == nil {
		return nil, fmt.Errorf("a recovery set is built from a kit")
	}
	if strings.TrimSpace(kit.ArtifactID) == "" {
		return nil, fmt.Errorf("a recovery set references its kit by immutable artifact id, and this kit has none")
	}
	if len(kit.Sealed) == 0 {
		return nil, fmt.Errorf("a recovery set may only be written for a sealed kit: an unsealed one has nothing a recovery could open")
	}
	return &Set{
		Binding:            kit.Binding,
		ArtifactID:         kit.ArtifactID,
		Fingerprints:       copySecrets(kit.Fingerprints),
		VeleroRepositoryID: kit.VeleroRepositoryID,
		S3Location:         kit.S3Location,
		Versions:           copySecrets(versions),
		Checksums:          map[string]string{"kit": kitDigest},
		WrittenAt:          now.UTC(),
	}, nil
}

// WithBackup returns the set with one backup recorded. A backup that did not
// complete is refused rather than recorded: the set is the list of things a
// recovery may start from, and a failed one is not one of them.
func (s *Set) WithBackup(b Backup) (*Set, error) {
	if !b.Completed() {
		return nil, fmt.Errorf("backup %s ended as %q: only a completed backup may be recorded in a recovery set", b.Name, b.Status)
	}
	out := *s
	out.Backups = append(append([]Backup(nil), s.Backups...), b)
	cp := copySecrets(s.Checksums)
	out.Checksums = cp
	return &out, nil
}

// Verify refuses a set that does not belong where the caller is.
func (s *Set) Verify(want Binding) error {
	if err := verifyBinding("recovery set", s.Binding, want); err != nil {
		return err
	}
	if s.ArtifactID == "" {
		return fmt.Errorf("this recovery set names no kit artifact, so it cannot say which kit opens its backups")
	}
	if got, want := s.Checksums["kit"], ""; got == want {
		return fmt.Errorf("this recovery set carries no checksum for kit %s, so an upload can never be verified against it", s.ArtifactID)
	}
	return nil
}

// Latest returns the newest Completed backup a recovery may start from, and
// whether there is one.
func (s *Set) Latest() (Backup, bool) {
	var best Backup
	found := false
	for _, b := range s.Backups {
		if !b.Completed() {
			continue
		}
		if !found || b.CompletedAt.After(best.CompletedAt) {
			best, found = b, true
		}
	}
	return best, found
}

// Document is the set as it is stored.
func (s *Set) Document() ([]byte, error) {
	doc, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding the recovery set: %w", err)
	}
	return append(doc, '\n'), nil
}

// LoadSet reads a stored recovery set back.
func LoadSet(doc []byte) (*Set, error) {
	var s Set
	if err := json.Unmarshal(doc, &s); err != nil {
		return nil, fmt.Errorf("this is not a recovery set document: %w", err)
	}
	if err := s.Binding.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Eligible reports whether a set may be offered as "latest". The kit must have
// been sealed and the upload must have completed: a partial or failed upload
// is never eligible.
func (s *Set) Eligible() bool { return s.Complete && len(s.Checksums["kit"]) > 0 }
