package install

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"kubenest.io/cli/pkg/storage"
)

// Status is a stage's terminal or opening state. These three strings are the
// journal's vocabulary AND the wire's (install_stage_update, kn-w051) — one
// vocabulary, two views. `converging` is deliberately absent: it lives inside
// a stage's converge.Wait loop and is printed, never recorded, because a
// journal of transient states is not a record of what happened.
type Status string

const (
	StatusStarted   Status = "started"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// Entry is one stage transition.
type Entry struct {
	Stage     string    `json:"stage"`
	Status    Status    `json:"status"`
	At        time.Time `json:"at"`
	Component string    `json:"component,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	// RunID ties an entry to one installer process, so a resumed run is
	// legible in the record rather than inferred from timestamps.
	RunID string `json:"run_id,omitempty"`
}

// Identity is what makes a resume a resume rather than a different install
// wearing the same journal. install.mdx's recovery path is "re-run the
// identical command"; this is "identical", made checkable.
type Identity struct {
	Cluster       string   `json:"cluster"`
	Bundle        string   `json:"bundle"`
	HATier        string   `json:"ha_tier"`
	Servers       []string `json:"servers"`
	Agents        []string `json:"agents,omitempty"`
	Profiles      []string `json:"profiles,omitempty"`
	StorageDevice string   `json:"storage_device,omitempty"`
}

// Normalized returns the identity with node and profile lists sorted, so
// argument ORDER is not mistaken for a different install.
func (i Identity) Normalized() Identity {
	i.Servers = sortedCopy(i.Servers)
	i.Agents = sortedCopy(i.Agents)
	i.Profiles = sortedCopy(i.Profiles)
	return i
}

// Differences names every field on which two identities disagree, in the
// user's terms. Empty means identical.
func (i Identity) Differences(other Identity) []string {
	a, b := i.Normalized(), other.Normalized()
	var diffs []string
	scalar := func(field, was, now string) {
		if was != now {
			diffs = append(diffs, fmt.Sprintf("%s: the journal has %q, you passed %q", field, was, now))
		}
	}
	list := func(field string, was, now []string) {
		if !slices.Equal(was, now) {
			diffs = append(diffs, fmt.Sprintf("%s: the journal has [%s], you passed [%s]",
				field, strings.Join(was, " "), strings.Join(now, " ")))
		}
	}
	scalar("cluster name", a.Cluster, b.Cluster)
	scalar("bundle", a.Bundle, b.Bundle)
	scalar("HA tier", a.HATier, b.HATier)
	scalar("--storage-device", a.StorageDevice, b.StorageDevice)
	list("servers", a.Servers, b.Servers)
	list("agents", a.Agents, b.Agents)
	list("profiles", a.Profiles, b.Profiles)
	return diffs
}

// ClusterRecord is what stage 2 (register) leaves behind: the NON-SECRET
// facts only.
//
// The agent JWT and the repository deploy key are minted once per process by
// design and live in memory until they terminate on the target hosts. Nothing
// here can hold them — that is why this struct has four scalar fields and no
// credential type, and why resume re-runs register instead of trying to
// recover secrets it deliberately never wrote down.
type ClusterRecord struct {
	ClusterID    string `json:"cluster_id,omitempty"`
	TokenVersion int    `json:"token_version,omitempty"`
	RepoURL      string `json:"repo_url,omitempty"`
	Adopted      bool   `json:"adopted,omitempty"`
}

// Ownership records who created the kubenest-vg volume group. It gates what
// uninstall may remove: a volume group the customer created is never removed,
// on either path.
//
// It is storage.Ownership rather than a type of its own, and storage's values
// are the backend's VolumeGroupOwnership enum. One vocabulary from the
// preflight check that decides it, through the journal that remembers it, to
// the record the control plane stores — a second spelling here is exactly how
// uninstall would end up reading a value it did not recognise and guessing.
type Ownership = storage.Ownership

// StorageRecord is stage 7's decision, and the only input uninstall trusts
// about block devices.
type StorageRecord struct {
	Device    string    `json:"device,omitempty"`
	Ownership Ownership `json:"ownership,omitempty"`
}

// Journal is the durable record of one install, and the reason resume is
// deterministic rather than a hope that every component happens to be
// idempotent (install.mdx, "When it fails").
type Journal struct {
	Identity Identity      `json:"identity"`
	Cluster  ClusterRecord `json:"cluster"`
	Storage  StorageRecord `json:"storage"`
	Entries  []Entry       `json:"entries"`

	path string
}

const (
	journalDirName = "journals"
	dirMode        = 0o700
	fileMode       = 0o600
)

// JournalPath is where one cluster's journal lives. It sits beside the CLI
// config, under the same 0700 directory.
func JournalPath(cluster string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kubenest", journalDirName, safeFileName(cluster)+".json"), nil
}

// safeFileName keeps a cluster name from escaping the journal directory. A
// name is user input and reaches a path.
func safeFileName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "cluster"
	}
	return b.String()
}

// OpenJournal loads the journal for this install, or starts one.
//
// A journal for a DIFFERENT install is refused rather than overwritten or
// silently reused: resuming into a half-installed cluster with changed flags
// is how an install ends up not matching its own record, and the record is
// what every day-2 operation trusts.
func OpenJournal(path string, want Identity) (*Journal, error) {
	want = want.Normalized()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Journal{Identity: want, path: path}, nil
	}
	if err != nil {
		return nil, err
	}
	var j Journal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("read install journal %s: %w\nthe journal is the record resume reads; move it aside and run `kubenest platform uninstall --confirm` to start from a known state", path, err)
	}
	j.path = path
	if diffs := j.Identity.Differences(want); len(diffs) > 0 {
		return nil, fmt.Errorf(
			"this journal records a different install:\n  %s\n\nresume re-runs the IDENTICAL command (%s). Fix the flag, or run `kubenest platform uninstall --confirm` on those hosts and install fresh",
			strings.Join(diffs, "\n  "), path)
	}
	// Keep the loaded identity; it and want are equal by the check above.
	return &j, nil
}

// Append records a transition and persists immediately. Persisting on every
// transition — not at the end — is what makes a killed installer resumable.
func (j *Journal) Append(e Entry) error {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	j.Entries = append(j.Entries, e)
	return j.Save()
}

// Completed reports whether a stage finished successfully in a previous run,
// which is the only condition under which the engine skips it.
func (j *Journal) Completed(stage string) (time.Time, bool) {
	for i := len(j.Entries) - 1; i >= 0; i-- {
		if j.Entries[i].Stage != stage {
			continue
		}
		// The LAST word on a stage wins: a stage that completed and was
		// later re-run into a failure is not complete.
		if j.Entries[i].Status == StatusCompleted {
			return j.Entries[i].At, true
		}
		return time.Time{}, false
	}
	return time.Time{}, false
}

// LastFailure returns the most recent failed entry, for the resume banner.
func (j *Journal) LastFailure() (Entry, bool) {
	for i := len(j.Entries) - 1; i >= 0; i-- {
		if j.Entries[i].Status == StatusFailed {
			return j.Entries[i], true
		}
	}
	return Entry{}, false
}

// Save writes the journal atomically: a temp file in the same directory, then
// a rename. A journal truncated by a crash mid-write would make resume read
// garbage about a cluster that exists.
func (j *Journal) Save() error {
	if j.path == "" {
		return nil // in-memory journal (tests)
	}
	dir := filepath.Dir(j.path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".journal-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if err := tmp.Chmod(fileMode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, j.path)
}

// Path returns where this journal is stored, for messages that tell an
// operator where to look.
func (j *Journal) Path() string { return j.path }

// Remove deletes the journal. Uninstall calls it last, once the hosts are
// actually clean — a journal outliving its cluster would make the next
// install refuse for a cluster that no longer exists.
func (j *Journal) Remove() error {
	if j.path == "" {
		return nil
	}
	if err := os.Remove(j.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// JournalDir is where every cluster's journal lives.
func JournalDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kubenest", journalDirName), nil
}

// ReadJournal loads a journal without checking it against a request. Used by
// uninstall, which does not have an install request to compare — it has a
// cluster it is taking apart.
func ReadJournal(path string) (*Journal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var j Journal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("read install journal %s: %w", path, err)
	}
	j.path = path
	return &j, nil
}
