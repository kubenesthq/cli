package operation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/k3s"
)

// The sentinel refusals. Every one of them is a refusal, never a retry: the
// whole point of the record being the lock is that the loser of a race learns
// it lost.
var (
	// ErrLocked is another operation holding the record. This refusal IS the
	// lock (PLAN 7.2).
	ErrLocked = errors.New("another operation holds the record")
	// ErrStale is a handle whose resourceVersion is no longer current: the
	// record moved on, and whatever the caller was about to write was derived
	// from a version of the record that no longer exists.
	ErrStale = errors.New("the record changed since it was read")
	// ErrNotOwner is a handle whose ownership token no longer owns the record
	// — a take-over happened. It is refused even when the resourceVersion the
	// handle read is still current, because ownership is not a version.
	ErrNotOwner = errors.New("this executor no longer owns the record")
	// ErrNoRecord is a lookup for an operation ID the cluster has no record
	// for, live or historical.
	ErrNoRecord = errors.New("no record for that operation")
	// ErrTakeOverRefused is a take-over the rules do not permit.
	ErrTakeOverRefused = errors.New("take-over refused")
	// ErrReconcile is a resume that stopped because an action's outcome could
	// not be established. It carries the reconciliation step to perform.
	ErrReconcile = errors.New("the outcome of an action could not be established")
	// errAlreadyExists is the API server refusing a create because the object
	// exists — the other half of the lock, for the race the pre-check misses.
	errAlreadyExists = errors.New("the record object already exists")
)

// Store is the record-as-lock: one ConfigMap in kube-system, read and written
// through `k3s kubectl` on a server node over the existing SSH transport.
//
// There is deliberately no k8s.io/client-go here. A client-go store would need
// a kubeconfig on the operator's laptop, and the rule this whole design
// respects is that key material and cluster admin credentials stay on the
// host.
type Store struct {
	// Runner is the SSH connection to a server node.
	Runner k3s.Runner
	// Operator names whoever holds a new token, e.g. "ana@laptop". It is what
	// a refused second laptop is told.
	Operator string
	// Now overrides the clock. Tests need it; heartbeats and staleness
	// readings are the only places the record depends on wall time.
	Now func() time.Time

	// Mirror is the control plane's copy of the record — the display and
	// `check_upgrade` path — or nil for a CLI that has none. Every write of the
	// live record is mirrored when it is set, and a mirror that fails is
	// reported on the handle and never fails the operation: the record in the
	// cluster is the lock, and it works while this control plane does not.
	Mirror *api.Client
	// MirrorClusterID is the control plane's id for the cluster this record is
	// about. The mirror route is addressed by it, while the record's own
	// Request.Cluster is the cluster's NAME. Empty means this CLI does not know
	// it (an operation before registration), and a configured mirror then
	// reports that rather than posting a record to the wrong cluster.
	MirrorClusterID string
	// MirrorTimeout overrides the bound on one mirror write. Zero uses
	// defaultMirrorTimeout.
	MirrorTimeout time.Duration
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// Stored is a record as the cluster holds it: the decoded record plus the
// resourceVersion the compare-and-swap is anchored on.
type Stored struct {
	Record          *Record
	ResourceVersion string
}

// Handle is one executor's ownership of the live record. Every write carries
// both the resourceVersion this handle read and its token; either being out of
// date is a refusal.
type Handle struct {
	store *Store
	mu    sync.Mutex
	rec   *Record
	rv    string
	token string
	// mirrorErr is the last mirror write's failure, or nil when the last one
	// succeeded. Guarded by mu, because writes through one handle may come from
	// more than one place.
	mirrorErr error
}

// OperationID is the operation this handle owns.
func (h *Handle) OperationID() string { return h.rec.OperationID }

// Token is the ownership token.
func (h *Handle) Token() string { return h.token }

// ResourceVersion is the version of the record this handle read last.
func (h *Handle) ResourceVersion() string { return h.rv }

// Record is the last version this handle saw. It is a read-only view: writes
// go through Update, which is the only path that carries the token and the
// resourceVersion forward.
func (h *Handle) Record() *Record { return h.rec }

// kubectl lines. Reads go through k3s.Kubectl; writes cannot, because the
// document travels over stdin and never in the command string (kn-40rd: a
// command string is the argv of the shell sshd spawns, readable in ps by every
// local user and recorded by whatever the host audits). The write lines are the
// same `sudo -n k3s kubectl …` shape k3s.ReplaceManifest uses.
const (
	getArgs = "get configmap %s -n " + Namespace + " -o json"
	// -o json on the write is what tells us the new resourceVersion: plain
	// `kubectl replace` prints "configmap/x replaced", which would leave the
	// handle unable to make its next write.
	createCmd  = "sudo -n k3s kubectl create -f - -o json"
	replaceCmd = "sudo -n k3s kubectl replace -f - -o json"
)

// Current reads the live record, or returns nil when the cluster has none.
func (s *Store) Current(ctx context.Context) (*Stored, error) {
	return s.read(ctx, Name)
}

// Find returns the record for one operation ID, live or historical.
//
// Historical matters: a completed operation's record stays findable by its own
// ID after a later operation has replaced the live object, which is what makes
// `--resume <operation-id>` name something rather than nothing.
func (s *Store) Find(ctx context.Context, opID string) (*Stored, error) {
	if !validOperationID(opID) {
		return nil, fmt.Errorf("operation id %q is not a usable identity (lowercase hex, 8-64 characters)", opID)
	}
	live, err := s.Current(ctx)
	if err != nil {
		return nil, err
	}
	if live != nil && live.Record.OperationID == opID {
		return live, nil
	}
	hist, err := s.read(ctx, HistoryPrefix+opID)
	if err != nil {
		return nil, err
	}
	if hist == nil {
		return nil, fmt.Errorf("%w: %s has neither a live record nor a finished one", ErrNoRecord, opID)
	}
	return hist, nil
}

// Acquire takes the lock: it creates the record, or replaces one that a
// previous operation left marked terminal, and refuses while another operation
// is live. The refusal is the lock.
//
// Nothing else in this package submits a remote action, so the record exists
// before the operation's first side effect by construction.
//
// Nothing in this package waits either, and that is deliberate: a --wait that
// held the lock while it waited would block every other operator for hours. A
// caller with a maintenance window acquires AFTER the window opens and repeats
// its safety gates against the state at that moment, so an interrupted wait
// holds nothing and can simply be started again (PLAN 7.2).
func (s *Store) Acquire(ctx context.Context, req Request) (*Handle, error) {
	now := s.now()
	rec := &Record{
		OperationID: NewOperationID(),
		Request:     req,
		Executor: Executor{
			Token:     NewToken(),
			Operator:  s.Operator,
			State:     ExecutorRunning,
			Heartbeat: now,
		},
		StartedAt: now,
		UpdatedAt: now,
	}
	if err := rec.Validate(); err != nil {
		return nil, err
	}

	live, err := s.Current(ctx)
	if err != nil {
		return nil, err
	}
	h := &Handle{store: s, rec: rec, token: rec.Executor.Token}
	switch {
	case live == nil:
		rv, err := s.create(ctx, Name, rec)
		if errors.Is(err, errAlreadyExists) {
			// Lost the create race: read who won and say so. This is the
			// narrow window the pre-check cannot close.
			return nil, s.refusedByOther(ctx, "creating")
		}
		if err != nil {
			return nil, err
		}
		h.rv = rv
	case live.Record.Terminal:
		// The old record is finished. Replacing it carries the
		// resourceVersion it was read at, so a concurrent replacement is a 409
		// and the 409 is the refusal.
		rv, err := s.replace(ctx, Name, rec, live.ResourceVersion)
		if err != nil {
			return nil, err
		}
		h.rv = rv
	default:
		return nil, locked(live.Record)
	}
	rec.Revision = h.rv
	// The record now exists and owns the operation; the mirror is display.
	s.mirror(ctx, h, rec)
	return h, nil
}

// Heartbeat records that this executor is still alive and on which stage. It is
// what tells a reader "running" from "interrupted"; it is never what decides a
// take-over.
func (h *Handle) Heartbeat(ctx context.Context, stage string) error {
	return h.store.Update(ctx, h, func(r *Record) error {
		r.Stage = stage
		r.Executor.Heartbeat = h.store.now()
		return nil
	})
}

// Update is the only write path: it reads the live record, refuses a handle
// that no longer owns it or whose resourceVersion is not current, applies the
// mutation, and replaces the object carrying the resourceVersion it read.
//
// The read is not decoration. A token is a field of the record, so the only
// place a mismatched token can be seen is the record itself — and a refusal
// that only fired on a 409 would let a taken-over handle through whenever the
// resourceVersion happened to match. The API server's 409 covers the window
// between this read and the replace.
func (s *Store) Update(ctx context.Context, h *Handle, mutate func(*Record) error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return s.update(ctx, h, mutate)
}

func (s *Store) update(ctx context.Context, h *Handle, mutate func(*Record) error) error {
	live, err := s.read(ctx, Name)
	if err != nil {
		return err
	}
	if live == nil {
		return fmt.Errorf("%w: the record was deleted while operation %s held it", ErrStale, h.rec.OperationID)
	}
	if live.Record.Executor.Token != h.token {
		return fmt.Errorf("%w: operation %s is held by %s, not by this executor", ErrNotOwner, live.Record.OperationID, holder(live.Record))
	}
	if live.ResourceVersion != h.rv {
		return fmt.Errorf("%w: this executor read resourceVersion %s and the cluster is at %s", ErrStale, h.rv, live.ResourceVersion)
	}
	rec := live.Record
	if err := mutate(rec); err != nil {
		return err
	}
	if rec.Executor.Token != h.token {
		return fmt.Errorf("refusing to write operation %s: an update may not change the ownership token (that is TakeOver)", rec.OperationID)
	}
	rec.UpdatedAt = s.now()
	rv, err := s.replace(ctx, Name, rec, h.rv)
	if err != nil {
		return err
	}
	rec.Revision = rv
	h.rec, h.rv = rec, rv
	s.mirror(ctx, h, rec)
	return nil
}

// Stop records that this executor has stopped. It is the second half of what a
// take-over requires, and it is the operator's assertion — the CLI cannot see a
// laptop that is asleep, so it never infers "stopped".
func (s *Store) Stop(ctx context.Context, h *Handle) error {
	return s.Update(ctx, h, func(r *Record) error {
		r.Executor.State = ExecutorStopped
		return nil
	})
}

// read returns the named object, or nil when it does not exist.
func (s *Store) read(ctx context.Context, name string) (*Stored, error) {
	out, err := k3s.Kubectl(ctx, s.Runner, fmt.Sprintf(getArgs, name))
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the operation record %s/%s: %w", Namespace, name, err)
	}
	stored, err := decodeStored(out)
	if err != nil {
		return nil, fmt.Errorf("reading the operation record %s/%s: %w", Namespace, name, err)
	}
	return stored, nil
}

// create writes the object for the first time.
func (s *Store) create(ctx context.Context, name string, rec *Record) (string, error) {
	doc, err := configMapDoc(name, rec, "")
	if err != nil {
		return "", err
	}
	out, err := s.run(ctx, createCmd, doc)
	if err != nil {
		if isAlreadyExists(err) {
			return "", fmt.Errorf("%w: %s/%s", errAlreadyExists, Namespace, name)
		}
		return "", err
	}
	return revisionOf(out)
}

// replace writes the object carrying the resourceVersion it was read at. The
// API server returns 409 on a mismatch, and that 409 IS the compare-and-swap.
func (s *Store) replace(ctx context.Context, name string, rec *Record, rv string) (string, error) {
	doc, err := configMapDoc(name, rec, rv)
	if err != nil {
		return "", err
	}
	out, err := s.run(ctx, replaceCmd, doc)
	if err != nil {
		// A conflict means someone else wrote between our read and this write;
		// a NotFound means the object is gone. Both are "the record moved".
		if isConflict(err) || isNotFound(err) {
			return "", fmt.Errorf("%w: %v", ErrStale, err)
		}
		return "", err
	}
	return revisionOf(out)
}

func (s *Store) run(ctx context.Context, command string, doc []byte) (string, error) {
	res, err := s.Runner.RunInput(ctx, command, bytes.NewReader(doc))
	if err != nil {
		return "", fmt.Errorf("writing the operation record: %w", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("writing the operation record: exit %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	return res.Stdout, nil
}

// refusedByOther builds the lock refusal from whoever actually holds the
// record. When even that read fails, the refusal still refuses — an
// unattributable "someone else has it" is worse than useless but it is never
// permission.
func (s *Store) refusedByOther(ctx context.Context, what string) error {
	live, err := s.Current(ctx)
	switch {
	case err != nil:
		return fmt.Errorf("%w: %s the record failed because another operation created it first, and it could not be read back", ErrLocked, what)
	case live == nil:
		return fmt.Errorf("%w: %s the record failed because another operation created it first", ErrLocked, what)
	default:
		return locked(live.Record)
	}
}

// locked is the refusal that names the other operator.
func locked(rec *Record) error {
	return &LockedError{
		OperationID: rec.OperationID,
		Kind:        rec.Request.Kind,
		Stage:       rec.Stage,
		Operator:    rec.Executor.Operator,
		StartedAt:   rec.StartedAt,
		Heartbeat:   rec.Executor.Heartbeat,
		State:       rec.Executor.State,
	}
}

func holder(rec *Record) string {
	if rec.Executor.Operator == "" {
		return "another executor"
	}
	return rec.Executor.Operator
}

// LockedError is the refusal that IS the lock. It names the other operation,
// who is running it and how far it got, because "another operation is running"
// is not something an operator can act on.
type LockedError struct {
	OperationID string
	Kind        Kind
	Stage       string
	Operator    string
	StartedAt   time.Time
	Heartbeat   time.Time
	State       ExecutorState
}

func (e *LockedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: operation %s (%s) is %s on this cluster",
		ErrLocked, e.OperationID, e.Kind, e.State)
	if e.Operator != "" {
		fmt.Fprintf(&b, ", held by %s", e.Operator)
	}
	if !e.StartedAt.IsZero() {
		fmt.Fprintf(&b, " since %s", e.StartedAt.UTC().Format(time.RFC3339))
	}
	if e.Stage != "" {
		fmt.Fprintf(&b, ", at stage %s", e.Stage)
	}
	if !e.Heartbeat.IsZero() {
		fmt.Fprintf(&b, ", last heartbeat %s", e.Heartbeat.UTC().Format(time.RFC3339))
	}
	b.WriteString(".\n  Only one disruptive operation may run at a time. Wait for it, or resume it\n  from where it stopped with --resume " + e.OperationID + ".")
	return b.String()
}

// Is makes errors.Is(err, ErrLocked) true.
func (e *LockedError) Is(target error) bool { return target == ErrLocked }

// cmObject is the slice of a ConfigMap this package reads.
type cmObject struct {
	Metadata struct {
		Name            string `json:"name"`
		Namespace       string `json:"namespace"`
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
}

func configMapDoc(name string, rec *Record, rv string) ([]byte, error) {
	raw, err := encodeRecord(rec)
	if err != nil {
		return nil, err
	}
	meta := map[string]any{
		"name":      name,
		"namespace": Namespace,
		"labels": map[string]string{
			"app.kubernetes.io/name":       "kubenest",
			"app.kubernetes.io/managed-by": "kubenest-cli",
			"kubenest.io/operation-id":     rec.OperationID,
			"kubenest.io/kind":             string(rec.Request.Kind),
		},
		"annotations": map[string]string{
			"kubenest.io/terminal": strconv.FormatBool(rec.Terminal),
			"kubenest.io/operator": rec.Executor.Operator,
		},
	}
	if rv != "" {
		meta["resourceVersion"] = rv
	}
	doc := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   meta,
		"data":       map[string]string{dataKey: string(raw)},
	}
	return json.Marshal(doc)
}

func decodeStored(out string) (*Stored, error) {
	var obj cmObject
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		return nil, fmt.Errorf("decoding the record object: %w", err)
	}
	rec, err := decodeRecord(obj.Data)
	if err != nil {
		return nil, err
	}
	if obj.Metadata.ResourceVersion != "" {
		rec.Revision = obj.Metadata.ResourceVersion
	}
	return &Stored{Record: rec, ResourceVersion: obj.Metadata.ResourceVersion}, nil
}

func revisionOf(out string) (string, error) {
	var obj cmObject
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		return "", fmt.Errorf("the write succeeded but its result could not be read, so this executor does not know its resourceVersion: %w", err)
	}
	if obj.Metadata.ResourceVersion == "" {
		return "", errors.New("the write succeeded but returned no resourceVersion, so this executor cannot make a compare-and-swap write")
	}
	return obj.Metadata.ResourceVersion, nil
}

// The API server's own words. The classification is by its message because
// that is the only thing a kubectl exit leaves us: there is no typed error
// behind `sudo -n k3s kubectl`.
func isNotFound(err error) bool      { return strings.Contains(err.Error(), "NotFound") }
func isAlreadyExists(err error) bool { return strings.Contains(err.Error(), "AlreadyExists") }
func isConflict(err error) bool {
	s := err.Error()
	return strings.Contains(s, "Conflict") || strings.Contains(s, "the object has been modified")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
