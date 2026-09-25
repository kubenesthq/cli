// Package operation is the record of one disruptive operation, which is also
// the lock that keeps two operators from driving the same cluster at once.
//
// One ConfigMap named kubenest-operation in kube-system is the whole
// mechanism: written before the operation's first side effect, replaced with a
// compare-and-swap on its resourceVersion, and refused while another operation
// is live. Creation or replacement failing while another operation is active
// IS the lock (PLAN-CLOSE-THE-GAP-2026-09 7.2). This is deliberately not a
// workflow engine: one object per running operation, and the deterministic
// resume pkg/stages already implements.
//
// Everything here reaches the cluster through k3s.Kubectl over the existing
// SSH transport. The CLI has no k8s.io/client-go and gains none: keeping the
// kubeconfig on the operator's laptop is the rule this package exists to
// respect.
package operation

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	// Name is the live record. There is exactly one per cluster, and the
	// object being singular is the lock.
	Name = "kubenest-operation"
	// Namespace is kube-system: every k3s cluster has it, the CLI never
	// removes it, and it is not a customer namespace.
	Namespace = "kube-system"
	// HistoryPrefix names a completed operation's copy. Objects are never
	// renamed, so a later operation's Acquire replaces the live object while
	// the finished one stays findable by its own ID under this prefix.
	HistoryPrefix = Name + "-"
	// dataKey is the ConfigMap key holding the record JSON. One key, because a
	// record split across keys can be read half-updated.
	dataKey = "record.json"
)

// Kind is the disruptive operation the record is about. It is part of the
// record because a resume must continue the same operation, and an upgrade
// record must never be adoptable by a restore.
type Kind string

const (
	KindUpgrade             Kind = "upgrade"
	KindControlPlaneUpgrade Kind = "control-plane-upgrade"
	KindNodeAdd             Kind = "node-add"
	KindNodeRemove          Kind = "node-remove"
	KindNodeReplace         Kind = "node-replace"
	KindNodeReboot          Kind = "node-reboot"
	KindRestoreNamespace    Kind = "restore-namespace"
	KindRestoreVolume       Kind = "restore-volume"
	KindDatastoreRollback   Kind = "datastore-rollback"
	KindHostRecovery        Kind = "host-recovery"
)

// RecordDestroysItself reports whether the operation can destroy its own
// record — a datastore rollback, a host recovery, a control-plane upgrade all
// run while the API server that holds this object may be going away. Those
// operations also write an encrypted copy of the record to the S3 target
// (s3.go); every other kind needs no copy, because its record outlives it.
func (k Kind) RecordDestroysItself() bool {
	switch k {
	case KindDatastoreRollback, KindHostRecovery, KindControlPlaneUpgrade:
		return true
	}
	return false
}

// State is the record's lifecycle, which is also the state the control plane
// displays. It is derived rather than stored: a record and its terminal flag
// disagreeing would be a record that lies about its own state.
type State string

const (
	StateRunning  State = "running"
	StateTerminal State = "terminal"
)

// Result is how a terminal record ended.
type Result string

const (
	ResultSucceeded Result = "succeeded"
	ResultFailed    Result = "failed"
)

// Artifact is one input the operation was planned against, by digest. An
// artifact that changed underneath is a different operation wearing the same
// name, which is what the digest is here to catch.
type Artifact struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// Target is one host and the cluster objects that identify what it holds.
//
// The UIDs are Kubernetes' own identities and they are not decoration: a host
// ID survives a reinstall and a Node UID does not, and a PVC restored to a
// different volume has a different UID. Recording them makes "the same
// request" checkable rather than assumed.
type Target struct {
	HostID  string   `json:"host_id"`
	NodeUID string   `json:"node_uid,omitempty"`
	PVCUIDs []string `json:"pvc_uids,omitempty"`
}

// Request is the operation's immutable request: what it was asked to do, to
// what, and with which artifacts. A resume continues this and cannot change
// it, because a resume into a half-finished cluster with changed targets is
// how a cluster ends up not matching its own record.
type Request struct {
	Kind      Kind              `json:"kind"`
	Cluster   string            `json:"cluster"`
	Targets   []Target          `json:"targets,omitempty"`
	Artifacts []Artifact        `json:"artifacts,omitempty"`
	Versions  map[string]string `json:"versions,omitempty"`
}

// normalized returns the request with every list in a canonical order, so that
// argument ORDER is never mistaken for a different operation — the same rule
// pkg/stages applies to a journal identity.
func (r Request) normalized() Request {
	out := r
	out.Targets = slices.Clone(r.Targets)
	for i := range out.Targets {
		out.Targets[i].PVCUIDs = slices.Clone(out.Targets[i].PVCUIDs)
		slices.Sort(out.Targets[i].PVCUIDs)
	}
	slices.SortFunc(out.Targets, func(a, b Target) int {
		if c := strings.Compare(a.HostID, b.HostID); c != 0 {
			return c
		}
		return strings.Compare(a.NodeUID, b.NodeUID)
	})
	out.Artifacts = slices.Clone(r.Artifacts)
	slices.SortFunc(out.Artifacts, func(a, b Artifact) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Digest, b.Digest)
	})
	if r.Versions != nil {
		out.Versions = make(map[string]string, len(r.Versions))
		for k, v := range r.Versions {
			out.Versions[k] = v
		}
	}
	return out
}

// Digest is the request's identity: the control plane mirrors it so a record
// that changed after it was acquired is visible as such, and a resume compares
// it so a changed request is refused by name.
func (r Request) Digest() string {
	raw, err := json.Marshal(r.normalized())
	if err != nil {
		// A Request is plain JSON types; this cannot fail in practice, and a
		// digest that is not a digest must not silently look like one.
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Differences names every field on which two requests disagree, in the
// operator's terms. Empty means identical.
func (r Request) Differences(other Request) []string {
	var diffs []string
	compare := func(field, was, now string) {
		if was != now {
			diffs = append(diffs, fmt.Sprintf("%s: the record has %q, you passed %q", field, was, now))
		}
	}
	compare("operation kind", string(r.Kind), string(other.Kind))
	compare("cluster", r.Cluster, other.Cluster)
	compare("targets", describeTargets(r.Targets), describeTargets(other.Targets))
	compare("artifacts", describeArtifacts(r.Artifacts), describeArtifacts(other.Artifacts))
	compare("versions", describeVersions(r.Versions), describeVersions(other.Versions))
	return diffs
}

func describeTargets(targets []Target) string {
	n := Request{Targets: targets}.normalized().Targets
	parts := make([]string, 0, len(n))
	for _, t := range n {
		parts = append(parts, t.HostID+"/"+t.NodeUID+"/"+strings.Join(t.PVCUIDs, ","))
	}
	return strings.Join(parts, " ")
}

func describeArtifacts(artifacts []Artifact) string {
	n := Request{Artifacts: artifacts}.normalized().Artifacts
	parts := make([]string, 0, len(n))
	for _, a := range n {
		parts = append(parts, a.Name+"@"+a.Digest)
	}
	return strings.Join(parts, " ")
}

func describeVersions(versions map[string]string) string {
	keys := make([]string, 0, len(versions))
	for k := range versions {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+versions[k])
	}
	return strings.Join(parts, " ")
}

// ExecutorState is what the previous executor is known to be doing. It is what
// a take-over is decided on, and it is deliberately an assertion by the
// operator rather than an observation: the CLI can see a heartbeat go stale but
// it cannot see a laptop that is asleep, so "stale" is never "stopped".
type ExecutorState string

const (
	// ExecutorRunning is an executor that has the record and has not said it
	// stopped.
	ExecutorRunning ExecutorState = "running"
	// ExecutorPaused is an executor between stages — an upgrade whose window
	// closed, waiting rather than failing. It still holds the record and it is
	// still going to come back, so a take-over is refused.
	ExecutorPaused ExecutorState = "paused"
	// ExecutorStopped is an executor the operator has stopped, which is one of
	// the two things a take-over requires (the other is reconciled actions).
	ExecutorStopped ExecutorState = "stopped"
)

// Executor is the ownership token and the executor behind it.
//
// The token is what makes "the same executor" checkable: every update must
// carry it, so a handle that has been taken over from cannot write even if the
// resourceVersion it read is still current.
type Executor struct {
	Token string `json:"token"`
	// Operator is who holds the token, in the operator's own words —
	// "ana@laptop". A refusal that does not name the other operator is
	// useless to the person reading it.
	Operator string `json:"operator"`
	// State is the previous executor's own assertion about itself.
	State ExecutorState `json:"state"`
	// Heartbeat is the last time that executor wrote anything. A stale
	// heartbeat is how the control plane reads "interrupted"; it is never a
	// permission to take over (7.2).
	Heartbeat time.Time `json:"heartbeat"`
}

// ActionStatus is where one remote action got to.
//
// The gap between Recorded and Submitted is the whole reason this type exists:
// an action recorded but never submitted never happened and is safe to repeat,
// while an action submitted whose outcome was never written back is the
// uncertain case a resume must reconcile instead of re-submitting.
type ActionStatus string

const (
	// ActionRecorded: written to the record, not yet sent. The process can die
	// here and the successor knows the action never ran.
	ActionRecorded ActionStatus = "recorded"
	// ActionSubmitted: sent, outcome not yet written back. This is the state a
	// killed CLI leaves behind, and the state a resume must establish.
	ActionSubmitted ActionStatus = "submitted"
	ActionSucceeded ActionStatus = "succeeded"
	ActionFailed    ActionStatus = "failed"
)

// ActionKind is the shape of the thing that was submitted, so a successor
// knows what it may safely look at.
type ActionKind string

const (
	ActionSSH     ActionKind = "ssh"
	ActionPlan    ActionKind = "plan"
	ActionRestore ActionKind = "restore"
)

// Action is one remote action: its stable identity, what a successor must
// observe to tell whether it happened, and how far it got.
//
// The command itself is deliberately NOT stored — only its hash. A command
// string is exactly where a credential ends up (kn-40rd), and the record is
// copied off-cluster and mirrored to the control plane, so a command in the
// record is a command in two more places.
type Action struct {
	// ID is the stable identity: a hash of the stage and the command, so the
	// same action has the same ID across attempts and across laptops.
	ID string `json:"id"`
	// Stage is the stage the action belongs to, in the operation's own words.
	Stage string `json:"stage,omitempty"`
	// Kind is ssh, plan or restore.
	Kind ActionKind `json:"kind"`
	// Postcondition is the sentence a successor reads to decide whether this
	// action happened. It is written down BEFORE the action is submitted.
	Postcondition string `json:"postcondition"`
	// Observe is a read-only command that exits zero iff the postcondition
	// holds. Empty means the outcome cannot be established by observation,
	// which makes a resume stop and name the reconciliation step.
	Observe     string       `json:"observe,omitempty"`
	Status      ActionStatus `json:"status"`
	At          time.Time    `json:"at"`
	SubmittedAt *time.Time   `json:"submitted_at,omitempty"`
	FinishedAt  *time.Time   `json:"finished_at,omitempty"`
	// Detail is sanitized remote output: never raw, and never a credential
	// (pkg/stages.Sanitize is applied where it is written).
	Detail string `json:"detail,omitempty"`
}

// WriteStatus is whether a write the operation owes has been discharged yet.
type WriteStatus string

const (
	WritePending WriteStatus = "pending"
	WriteDone    WriteStatus = "done"
)

// PendingWrite is a write the operation still owes: the host inventory and the
// recovery metadata are written outside the operation's own steps, so a host
// that joined but whose inventory write was interrupted stays identifiable
// from the record. Completion then distinguishes finished host work from
// pending inventory and recovery-metadata writes instead of reading as if
// nothing were owed.
type PendingWrite struct {
	// Kind is what is owed: "inventory" or "recovery-metadata".
	Kind   string      `json:"kind"`
	Target string      `json:"target"`
	Detail string      `json:"detail,omitempty"`
	Status WriteStatus `json:"status"`
	At     time.Time   `json:"at"`
	DoneAt *time.Time  `json:"done_at,omitempty"`
}

// Record is the whole record: the immutable request, who owns it, how far each
// action got, and what is still owed.
//
// It is JSON only, and it holds no field a credential can sit in — see
// Validate. This object is copied to the S3 target and mirrored to the control
// plane, neither of which is the customer's cluster.
type Record struct {
	OperationID string         `json:"operation_id"`
	Request     Request        `json:"request"`
	Executor    Executor       `json:"executor"`
	Stage       string         `json:"stage,omitempty"`
	Terminal    bool           `json:"terminal"`
	Result      string         `json:"result,omitempty"`
	Actions     []Action       `json:"actions,omitempty"`
	Pending     []PendingWrite `json:"pending,omitempty"`
	StartedAt   time.Time      `json:"started_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	// Revision is the resourceVersion this record was last written at. It
	// travels inside the JSON so the mirror route can refuse a record revision
	// older than the one it already holds without re-reading the ConfigMap.
	Revision string `json:"revision,omitempty"`
}

// State is the record's lifecycle state.
func (r *Record) State() State {
	if r.Terminal {
		return StateTerminal
	}
	return StateRunning
}

// Outstanding returns the writes the operation still owes.
func (r *Record) Outstanding() []PendingWrite {
	var out []PendingWrite
	for _, w := range r.Pending {
		if w.Status != WriteDone {
			out = append(out, w)
		}
	}
	return out
}

// ActionByID finds one action.
func (r *Record) ActionByID(id string) (Action, bool) {
	for _, a := range r.Actions {
		if a.ID == id {
			return a, true
		}
	}
	return Action{}, false
}

// Validate refuses a record that must not be written to a cluster, an S3
// target or the control plane: one without an identity or an owner cannot be
// the lock, and one carrying a credential-shaped field name is refused before
// it is copied anywhere.
//
// The field-name scan is a tripwire, not a redaction: it catches a field added
// carelessly to this type, and it does NOT make caller-supplied text safe.
// Text that came from a remote shell is sanitized where it is written
// (pkg/stages.Sanitize), because that is the only place that knows it is
// remote output.
func (r *Record) Validate() error {
	if !validOperationID(r.OperationID) {
		return fmt.Errorf("operation id %q is not a usable identity (lowercase hex, 8-64 characters): it names the record and its history copy", r.OperationID)
	}
	if r.Request.Kind == "" {
		return fmt.Errorf("operation %s has no kind: an upgrade record must never be adoptable by a restore", r.OperationID)
	}
	if r.Request.Cluster == "" {
		return fmt.Errorf("operation %s names no cluster", r.OperationID)
	}
	for i, t := range r.Request.Targets {
		if t.HostID == "" {
			return fmt.Errorf("operation %s: target %d has no host id, so a resume cannot tell which host it was", r.OperationID, i)
		}
	}
	if r.Executor.Token == "" {
		return fmt.Errorf("operation %s has no ownership token: an update that cannot be attributed cannot be refused", r.OperationID)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encoding operation %s: %w", r.OperationID, err)
	}
	if bad := credentialFieldsIn(raw); len(bad) > 0 {
		return fmt.Errorf("operation %s carries credential-shaped field(s) %s: this record is copied off-cluster and mirrored to the control plane", r.OperationID, strings.Join(bad, ", "))
	}
	return nil
}

// credentialFields are JSON field names the record must never have. The
// ownership token is deliberately absent: it is not a secret, it is the name of
// the executor, and a record that cannot say who owns it cannot refuse anyone.
var credentialFields = map[string]bool{
	"password":        true,
	"passphrase":      true,
	"secret":          true,
	"secrets":         true,
	"privatekey":      true,
	"accesskey":       true,
	"accesskeyid":     true,
	"secretaccesskey": true,
	"secretkey":       true,
	"apikey":          true,
	"jwt":             true,
	"bearer":          true,
	"credential":      true,
	"credentials":     true,
	"tokenhash":       true,
	"clientsecret":    true,
	"encryptionkey":   true,
	"deploykey":       true,
	"kubeconfig":      true,
}

// credentialFieldsIn walks every key of a JSON document, at any depth.
func credentialFieldsIn(raw []byte) []string {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	var bad []string
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				child := path + "." + k
				if credentialFields[normalizeKey(k)] {
					bad = append(bad, child)
				}
				walk(t[k], child)
			}
		case []any:
			for i, item := range t {
				walk(item, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(doc, "")
	return bad
}

func normalizeKey(k string) string {
	k = strings.ToLower(k)
	return strings.NewReplacer("-", "", "_", "", ".", "").Replace(k)
}

// ActionID is an action's stable identity: the stage and the command, hashed.
// The same action has the same ID across attempts and across laptops, which is
// what lets a successor recognise one it already carried out.
//
// The version prefix is there so that a future change to what goes into the
// identity does not silently collide with identities already written down.
func ActionID(stage, command string) string {
	sum := sha256.Sum256([]byte("kubenest-action-v1\x00" + stage + "\x00" + command))
	return hex.EncodeToString(sum[:16])
}

// validOperationID keeps an operation ID usable as a ConfigMap name suffix and
// as something safe to put in a kubectl command line. An ID arrives from a
// command-line flag.
func validOperationID(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// NewOperationID mints the identity of one operation: constant across its
// stages and across every resume, because it is what a successor names.
func NewOperationID() string { return randomHex() }

// NewToken mints an ownership token. It is not a credential: it is a name, and
// its only job is to make "is this still my record" checkable.
func NewToken() string { return randomHex() }

func randomHex() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice, and an operation that cannot
		// name itself must not proceed on a guess.
		panic("operation: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// encodeRecord marshals a record the way every writer writes it.
func encodeRecord(rec *Record) ([]byte, error) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("encoding operation %s: %w", rec.OperationID, err)
	}
	return raw, nil
}

// decodeRecord reads a record back out of a ConfigMap's data.
func decodeRecord(data map[string]string) (*Record, error) {
	raw, ok := data[dataKey]
	if !ok {
		return nil, fmt.Errorf("the record object has no %q key", dataKey)
	}
	var rec Record
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil, fmt.Errorf("decoding the record: %w", err)
	}
	return &rec, nil
}
