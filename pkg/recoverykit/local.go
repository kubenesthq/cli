package recoverykit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// localDirName is the directory under the operator's config directory that
// holds the kits written on this machine. It sits beside the install journal
// on purpose: a kit and the journal that explains it are one thing, and an
// operator looking for one after a disaster finds the other.
const localDirName = "recovery-kits"

// LocalDir is where this operator's kits live: under the config directory,
// next to the install journal.
func LocalDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kubenest", localDirName), nil
}

// LocalPath is where one kit is written locally:
//
//	~/.kubenest/recovery-kits/<cluster-id>/<kind>-<artifact-id>.json
//
// The file is the sealed kit exactly as it was uploaded — header and
// ciphertext — which is what makes the digest comparison in a later install
// meaningful: the local copy and the uploaded copy are the same bytes.
func LocalPath(clusterID string, kind Kind, artifactID string) (string, error) {
	dir, err := LocalDir()
	if err != nil {
		return "", err
	}
	if clusterID == "" || artifactID == "" {
		return "", errors.New("a local kit path needs a cluster id and an artifact id")
	}
	if strings.ContainsAny(clusterID, "/\\") || strings.ContainsAny(artifactID, "/\\") {
		return "", fmt.Errorf("neither %q nor %q may contain a path separator", clusterID, artifactID)
	}
	return filepath.Join(dir, clusterID, string(kind)+"-"+artifactID+".json"), nil
}

// NewestLocal reads the newest kit of one scope written on this machine.
//
// It is how a resumed install finds the kit an earlier run wrote and
// verified, without holding it in memory: the ordering guarantee is about what
// is on the disk and in the bucket, not about what this process happens to
// remember.
func NewestLocal(clusterID string, kind Kind) (*Kit, error) {
	dir, err := LocalDir()
	if err != nil {
		return nil, err
	}
	clusterDir := filepath.Join(dir, clusterID)
	entries, err := os.ReadDir(clusterDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no recovery kit for cluster %s has been written on this machine (%s does not exist): the kit is written by the install before anything can take a backup", clusterID, clusterDir)
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), string(kind)+"-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no %s recovery kit for cluster %s has been written on this machine", kind, clusterID)
	}
	// Sorted for a deterministic answer when two kits share a timestamp.
	sort.Strings(names)
	var newest *Kit
	for _, name := range names {
		doc, err := os.ReadFile(filepath.Join(clusterDir, name))
		if err != nil {
			return nil, err
		}
		kit, err := Load(doc)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(clusterDir, name), err)
		}
		if newest == nil || kit.WrittenAt.After(newest.WrittenAt) {
			newest = kit
		}
	}
	return newest, nil
}
