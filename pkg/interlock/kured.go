// Package interlock is the CLI's half of kured's own lock: the primitive that
// stops a CLI operation and an automatic reboot from both taking the same node
// down (plan 7.2, "Excluding kured").
//
// kured records its lock as a JSON annotation on its own DaemonSet and takes
// it with a read-modify-write that fails on a resourceVersion conflict
// (upstream pkg/daemonsetlock, verified at the pinned tag 1.23.0). This
// package speaks the same protocol over k3s.Kubectl, so the CLI and kured
// contend through ONE object: whoever writes first wins, the other is refused
// by the API server rather than by a convention, and a CLI that dies leaves
// nothing that outlives its TTL.
//
// Probe P1 (2026-09-25, real hardware, chart 6.1.0 = kured 1.23.0) is the
// evidence this package is built on: it is recorded, finding by finding, in
// the package comment of kured_test.go, beside the tests that hold it, and
// every rule below exists because one of those findings demanded it.
package interlock

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
)

// LockAnnotation is the annotation kured records its lock under, on its own
// DaemonSet. It is kured's default (--lock-annotation) and the chart the CLI
// installs does not change it, so both actors read one object.
const LockAnnotation = "weave.works/kured-node-lock"

// DaemonSetName and DaemonSetNamespace locate kured's DaemonSet, which is
// where the lock lives: the object kured itself reads and writes its lock on.
const (
	DaemonSetName      = "kured"
	DaemonSetNamespace = "kube-system"
)

// NodeMeta is the metadata kured records beside the lock: whether the node was
// ALREADY unschedulable when the lock was taken. It is what a release consults
// before uncordoning (kured 1.23.0 does exactly this), so a node cordoned for
// some other reason is not silently made schedulable again.
type NodeMeta struct {
	Unschedulable bool `json:"unschedulable"`
}

// Value is the lock document in kured's own JSON shape — a wire format, not an
// internal type. TTL is a time.Duration, which encodes as nanoseconds: the
// encoding kured writes and the field its expiry rule reads back. Changing the
// shape here makes kured's json.Unmarshal fail and its Holding() return an
// error, so kured would stop rebooting anything at all.
type Value struct {
	NodeID   string        `json:"nodeID"`
	Metadata NodeMeta      `json:"metadata,omitempty"`
	Created  time.Time     `json:"created"`
	TTL      time.Duration `json:"TTL"`
}

// LockTTLFor derives the lock lifetime from the bundle manifest.
//
// The longest operation that holds the lock is one node's reboot — cordon and
// drain within limits.timeouts.node-drain, then reboot and return within
// limits.timeouts.node-reboot (plan 7.3) — so the TTL is their sum. Both
// numbers come from the manifest and neither is a constant here: a hardcoded
// TTL would outlive the measurement that produced the timeouts it is meant to
// exceed. A missing timeout stays an error (pkg/manifest's rule), because a
// defaulted TTL is one nobody can audit.
func LockTTLFor(bundle *manifest.Manifest) (time.Duration, error) {
	drain, err := bundle.Limits.Timeouts.For("node-drain")
	if err != nil {
		return 0, err
	}
	reboot, err := bundle.Limits.Timeouts.For("node-reboot")
	if err != nil {
		return 0, err
	}
	return drain + reboot, nil
}

// maxConflictRetries bounds the compare-and-swap loop. kured retries a
// conflict for up to five minutes inside a daemon that runs for ever; the CLI
// bounds it instead, because its caller has a deadline and an operation that
// cannot take the lock should say so while an operator is still watching
// rather than sit in a loop holding a stage.
const maxConflictRetries = 5

// nodeName is what a Kubernetes node name may be (an RFC 1123 subdomain). It
// is checked before a node name reaches a command string, because k3s.Kubectl
// builds a shell command: a name carrying a shell metacharacter would run as
// several commands on the target host, with sudo.
var nodeName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// ValidNodeName reports whether name can be a node name and is therefore safe
// to place in a kubectl argument.
func ValidNodeName(name string) bool {
	return len(name) <= 253 && nodeName.MatchString(name)
}

// Acquire takes kured's lock for nodeID, returning whether nodeID holds it
// afterwards and, when it does not, who does. It is the CLI's side of P1's
// first finding.
//
// kured's own rule is followed exactly: a lock that is present and has not
// expired is honoured whoever wrote it (so a second CLI, or kured, is refused
// rather than overwritten), and a lock whose created + TTL has passed is taken
// over (P1's fourth finding: an orphaned lock must not stop the cluster for
// ever). nodeID must be the node name kured sees.
//
// ttl is the lifetime stamped on the lock — what LockTTLFor derives from the
// bundle manifest, and the same value the kured chart carries as
// configuration.lockTtl, so the CLI's hold and kured's own locks expire on one
// rule. It is an argument rather than package state because it has exactly one
// source per call and nothing that holds a lock may depend on a value somebody
// else was supposed to have set. A non-positive ttl is REFUSED: a lock that
// never expires is how one node that reboots and never becomes Ready stops
// every other node's automatic reboot for ever (P1, finding 4), and how a
// killed laptop becomes a fleet-wide reboot stop.
//
// The node's current schedulability is recorded in the lock's metadata, the
// way kured records it, so whoever releases the lock knows whether the node
// was schedulable before this operation cordoned it.
func Acquire(ctx context.Context, r k3s.Runner, nodeID string, ttl time.Duration) (held bool, holder string, err error) {
	if !ValidNodeName(nodeID) {
		return false, "", fmt.Errorf("interlock: %q is not a node name", nodeID)
	}
	if ttl <= 0 {
		return false, "", fmt.Errorf("interlock: a lock with no TTL would never expire, and one node that never returns would then stop every other node's reboot for ever (pass the bundle's limits.timeouts as LockTTLFor derives them)")
	}

	for attempt := 0; attempt < maxConflictRetries; attempt++ {
		st, err := readLock(ctx, r)
		if err != nil {
			return false, "", err
		}
		if st.hasLock && !Expired(st.lock, time.Now()) {
			return st.lock.NodeID == nodeID, st.lock.NodeID, nil
		}

		// We are about to take it, so the node's state now is the state to
		// record.
		unschedulable, err := nodeUnschedulable(ctx, r, nodeID)
		if err != nil {
			return false, "", err
		}
		mine := Value{
			NodeID:   nodeID,
			Metadata: NodeMeta{Unschedulable: unschedulable},
			Created:  time.Now().UTC(),
			TTL:      ttl,
		}
		err = writeLock(ctx, r, st, &mine)
		if err == nil {
			return true, nodeID, nil
		}
		if !isConflict(err) {
			return false, "", err
		}
		// The object moved between our read and our write: a second actor has
		// the lock in hand. Re-read and decide against what is there NOW;
		// re-sending the document we already know is stale is the double take
		// this retry exists to avoid.
	}
	return false, "", fmt.Errorf("interlock: kured's lock changed under every one of %d attempts: another actor is taking nodes down", maxConflictRetries)
}

// Holding reports whether nodeID currently holds kured's lock — present, not
// expired, and naming nodeID — and returns the lock document whenever the
// annotation is there.
//
// The document is returned even when the lock is not held (expired, or another
// node's, or absent, in which case it is the zero Value). A caller that is
// taking a node OUT of kured's pool needs the document to release a lock that
// node owns — including one that has since expired — and needs NodeID to tell
// "ours" from "another node's, leave it alone" (day2.HoldAutomaticReboots).
func Holding(ctx context.Context, r k3s.Runner, nodeID string) (held bool, lock Value, err error) {
	if !ValidNodeName(nodeID) {
		return false, Value{}, fmt.Errorf("interlock: %q is not a node name", nodeID)
	}
	st, err := readLock(ctx, r)
	if err != nil {
		return false, Value{}, err
	}
	if !st.hasLock {
		return false, Value{}, nil
	}
	return st.lock.NodeID == nodeID && !Expired(st.lock, time.Now()), st.lock, nil
}

// Release removes kured's lock, and only when the lock names nodeID: a lock
// another node holds is never touched, however stale it looks, because the
// node holding it may be mid-reboot. Taking over an expired lock that is not
// ours is a different operation with a different name (Acquire).
func Release(ctx context.Context, r k3s.Runner, nodeID string) error {
	if !ValidNodeName(nodeID) {
		return fmt.Errorf("interlock: %q is not a node name", nodeID)
	}
	for attempt := 0; attempt < maxConflictRetries; attempt++ {
		st, err := readLock(ctx, r)
		if err != nil {
			return err
		}
		if !st.hasLock {
			return fmt.Errorf("interlock: kured's lock is not held: there is nothing to release")
		}
		if st.lock.NodeID != nodeID {
			return fmt.Errorf("interlock: kured's lock is held by %s, not %s: release only ever removes our own", st.lock.NodeID, nodeID)
		}
		err = writeLock(ctx, r, st, nil)
		if err == nil {
			return nil
		}
		if !isConflict(err) {
			return err
		}
	}
	return fmt.Errorf("interlock: kured's lock changed under every one of %d release attempts", maxConflictRetries)
}

// Expired reports whether a lock has outlived the TTL it was written with.
// This is kured's own rule at 1.23.0, verbatim (ttlExpired): a zero TTL never
// expires. It is exported because the same question is asked by callers that
// read a lock through Holding.
func Expired(v Value, now time.Time) bool {
	return v.TTL > 0 && now.Sub(v.Created) >= v.TTL
}

// lockState is one read of kured's DaemonSet: the document exactly as it
// arrived (so a write replaces what was read), the resourceVersion that makes
// that write a compare-and-swap, and the lock annotation, if kured has one.
type lockState struct {
	doc     map[string]any
	rv      string
	lock    Value
	hasLock bool
}

// readLock reads kured's DaemonSet and the lock it carries.
func readLock(ctx context.Context, r k3s.Runner) (lockState, error) {
	out, err := k3s.Kubectl(ctx, r, "get daemonset -n "+DaemonSetNamespace+" "+DaemonSetName+" -o json")
	if err != nil {
		return lockState{}, err
	}
	// Decoded as generic JSON, with numbers kept as json.Number: every write
	// is a WHOLE-OBJECT replace, so a typed struct would silently drop each
	// field this package does not name (uid, generation, managedFields, the
	// pod template) and a float64 round trip would rewrite large integers.
	// Taking a lock must not rewrite kured's DaemonSet.
	dec := json.NewDecoder(strings.NewReader(out))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return lockState{}, fmt.Errorf("interlock: kured's DaemonSet did not parse as JSON: %w", err)
	}
	meta, _ := doc["metadata"].(map[string]any)
	if meta == nil {
		return lockState{}, fmt.Errorf("interlock: kured's DaemonSet carries no metadata")
	}
	rv, _ := meta["resourceVersion"].(string)
	if rv == "" {
		return lockState{}, fmt.Errorf("interlock: kured's DaemonSet carries no metadata.resourceVersion: a lock write without one cannot lose an update, and losing no update is the whole mechanism")
	}
	st := lockState{doc: doc, rv: rv}
	if raw := annotation(meta, LockAnnotation); raw != "" {
		if err := json.Unmarshal([]byte(raw), &st.lock); err != nil {
			return lockState{}, fmt.Errorf("interlock: annotation %s is not kured's lock document: %w", LockAnnotation, err)
		}
		st.hasLock = true
	}
	return st, nil
}

// writeLock replaces kured's DaemonSet with the document that was read, its
// lock annotation set to lock or removed when lock is nil.
//
// The replace carries the observed resourceVersion through `kubectl replace -f
// -`, which is what makes it a compare-and-swap: a kured that took the lock
// between our read and our write is answered with a Conflict instead of being
// overwritten. A merge patch would carry no resourceVersion, silently replace
// kured's lock with ours, and leave BOTH actors believing they may cordon and
// drain a node — the lost update this whole package exists to prevent.
func writeLock(ctx context.Context, r k3s.Runner, st lockState, lock *Value) error {
	meta, _ := st.doc["metadata"].(map[string]any)
	if meta == nil {
		return fmt.Errorf("interlock: kured's DaemonSet carries no metadata")
	}
	ann, _ := meta["annotations"].(map[string]any)
	if ann == nil {
		ann = map[string]any{}
	}
	if lock == nil {
		delete(ann, LockAnnotation)
	} else {
		b, err := json.Marshal(*lock)
		if err != nil {
			return fmt.Errorf("interlock: render the lock: %w", err)
		}
		ann[LockAnnotation] = string(b)
	}
	if len(ann) == 0 {
		delete(meta, "annotations")
	} else {
		meta["annotations"] = ann
	}
	b, err := json.Marshal(st.doc)
	if err != nil {
		return fmt.Errorf("interlock: render kured's DaemonSet: %w", err)
	}
	return k3s.ReplaceManifest(ctx, r, b)
}

// annotation reads one string annotation out of metadata.
func annotation(meta map[string]any, key string) string {
	ann, _ := meta["annotations"].(map[string]any)
	s, _ := ann[key].(string)
	return s
}

// isConflict reports whether a write was refused because the object moved
// under us. kubectl prints the API server's status error, whose reason is
// "Conflict" ("the object has been modified; please apply your changes to the
// latest version and try again") — the sentence P1 saw from the CLI's stale
// copy.
func isConflict(err error) bool {
	s := err.Error()
	return strings.Contains(s, "Conflict") || strings.Contains(s, "the object has been modified")
}

// nodeUnschedulable reads whether a node is currently cordoned. It is what a
// lock's metadata records, and it decides whether a release uncordons: kured
// at 1.23.0 uncordons a node on release only when the lock says the node was
// schedulable before kured took it.
func nodeUnschedulable(ctx context.Context, r k3s.Runner, nodeID string) (bool, error) {
	out, err := k3s.Kubectl(ctx, r, "get node "+nodeID+" -o json")
	if err != nil {
		return false, err
	}
	var node struct {
		Spec struct {
			Unschedulable bool `json:"unschedulable"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(out), &node); err != nil {
		return false, fmt.Errorf("interlock: node %s did not parse as JSON: %w", nodeID, err)
	}
	return node.Spec.Unschedulable, nil
}
