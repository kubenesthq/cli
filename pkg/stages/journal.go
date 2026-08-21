package stages

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
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

// Identity is what makes a resume a resume rather than a different operation
// wearing the same journal. install.mdx's recovery path is "re-run the
// identical command"; this is "identical", made checkable.
//
// Kind separates an install journal from an upgrade one so neither can ever
// resume into the other, and Fields carries whatever each operation considers
// part of its identity — the node set and profile list for an install, the
// version transition for an upgrade.
type Identity struct {
	// Kind is the operation, e.g. "install" or "upgrade".
	Kind string `json:"kind"`
	// Cluster is the cluster name the operation targets.
	Cluster string `json:"cluster"`
	// Fields are the operation's remaining identity, in the operator's own
	// terms: the map keys are what a mismatch message will name.
	Fields map[string]string `json:"fields,omitempty"`
}

// Differences names every field on which two identities disagree, in the
// user's terms. Empty means identical.
func (i Identity) Differences(other Identity) []string {
	var diffs []string
	compare := func(field, was, now string) {
		if was != now {
			diffs = append(diffs, fmt.Sprintf("%s: the journal has %q, you passed %q", field, was, now))
		}
	}
	compare("operation", i.Kind, other.Kind)
	compare("cluster name", i.Cluster, other.Cluster)

	keys := map[string]bool{}
	for k := range i.Fields {
		keys[k] = true
	}
	for k := range other.Fields {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, name := range names {
		compare(name, i.Fields[name], other.Fields[name])
	}
	return diffs
}

// List renders a list field for an Identity, sorted, so argument ORDER is
// never mistaken for a different operation.
func List(values []string) string {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return strings.Join(sorted, " ")
}

// Journal is the durable record of one operation, and the reason resume is
// deterministic rather than a hope that every step happens to be idempotent
// (install.mdx, "When it fails").
type Journal struct {
	Identity Identity `json:"identity"`
	// ClusterID is the control-plane id, once the operation knows it. Both
	// operations need it and it is not a secret.
	ClusterID string  `json:"cluster_id,omitempty"`
	Entries   []Entry `json:"entries"`
	// State is whatever else the operation must remember across a resume,
	// encoded by the caller. It exists so the engine can stay ignorant of
	// what an install or an upgrade considers worth remembering — and so
	// that nothing here has a field a credential could fit into.
	State json.RawMessage `json:"state,omitempty"`

	path string
}

// SetState stores the caller's record and persists the journal.
func (j *Journal) SetState(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	j.State = raw
	return j.Save()
}

// DecodeState reads the caller's record back. A journal with no state leaves
// v untouched and returns nil, so a first run needs no special case.
func (j *Journal) DecodeState(v any) error {
	if len(j.State) == 0 {
		return nil
	}
	return json.Unmarshal(j.State, v)
}

const (
	journalDirName = "journals"
	dirMode        = 0o700
	fileMode       = 0o600
)

// JournalPath is where one operation's journal for one cluster lives. It sits
// beside the CLI config, under the same 0700 directory. The kind is part of
// the name so an upgrade journal can never be opened as an install one.
func JournalPath(kind, cluster string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	name := safeFileName(cluster)
	if kind != "" && kind != "install" {
		// install keeps the historical <cluster>.json name so journals
		// written before upgrades existed still resume.
		name += "-" + safeFileName(kind)
	}
	return filepath.Join(home, ".kubenest", journalDirName, name+".json"), nil
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

// OpenJournal loads the journal for this operation, or starts one.
//
// A journal for a DIFFERENT operation is refused rather than overwritten or
// silently reused: resuming into a half-finished cluster with changed
// arguments is how a cluster ends up not matching its own record, and the
// record is what every day-2 operation trusts.
func OpenJournal(path string, want Identity) (*Journal, error) {
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
			"this journal records a different %s:\n  %s\n\nresume re-runs the IDENTICAL command (%s). Fix the argument, or start from a known state",
			j.Identity.Kind, strings.Join(diffs, "\n  "), path)
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
// operation refuse for a cluster that no longer exists.
func (j *Journal) Remove() error {
	if j.path == "" {
		return nil
	}
	if err := os.Remove(j.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
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
