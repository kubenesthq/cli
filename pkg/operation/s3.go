package operation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ObjectWriter is the slice of an S3 client this package needs: one
// unconditional PUT for the encrypted copy, one conditional PUT for the
// recovery-owner object.
//
// The real implementation is *s3.Client (kubenest-cli/pkg/s3), and NewCopy is
// where it is handed in. The interface stays rather than being replaced by that
// type because the dependency runs the other way: pkg/s3 and pkg/recoverykit
// assert against these interfaces in their own tests, so they import this
// package. Importing them back would close the cycle and `go test ./pkg/s3/`
// would stop building — the interface is the direction of that dependency, not
// a placeholder for a client that does not exist.
//
// PutIfAbsent is `If-None-Match: *` — a real conditional create — and its bool
// reports whether THIS call created the object.
type ObjectWriter interface {
	Put(ctx context.Context, key string, body []byte) error
	PutIfAbsent(ctx context.Context, key string, body []byte) (created bool, err error)
}

// Sealer encrypts the record copy. The fleet recovery key (T4.6) owns the only
// key that can open it, and this package deliberately never sees one: the copy
// is written to a target the customer's cluster does not control.
// *recoverykit.Sealer is the implementation.
type Sealer interface {
	Seal(plaintext []byte) ([]byte, error)
}

var (
	// ErrNoConditionalWrites is a target that cannot create an object
	// conditionally. Probe P3 (T4.8/T4.9) settles whether the target supports
	// it; until it does, automated recovery on such a target requires the
	// operator to acknowledge the documented single-operator rule (F20), and
	// the CLI must not quietly substitute an unconditional write.
	ErrNoConditionalWrites = errors.New("the target does not support a conditional create")
	// ErrAlreadyClaimed is a recovery-owner object that already exists: someone
	// else holds it, and elapsed time does not change that.
	ErrAlreadyClaimed = errors.New("the recovery-owner object already exists")
)

// Copy is the off-cluster copy of an operation's record.
//
// It exists for the operations that can destroy their own record — a datastore
// rollback, a host recovery, a control-plane upgrade — because a record that
// lives only in kube-system dies with the API server it was protecting.
//
// The copy is EVIDENCE, not a lock. Nothing about writing it excludes anyone;
// when the cluster's API is gone, the only thing that does is Claim.
//
// A Copy is built with NewCopy, which is where the writer and the sealer are
// checked; a struct literal with a fake writer and a fake sealer is how a test
// drives the copy without a bucket.
type Copy struct {
	Writer ObjectWriter
	Sealer Sealer
	// Prefix is the key prefix copies and claims live under.
	Prefix string
}

// NewCopy builds the off-cluster copy of one operation's record from the two
// pieces it needs: the S3 client for the target, and the sealer that encrypts
// to the fleet recipient (pkg/s3's *Client and pkg/recoverykit's Sealer,
// handed in by the CLI layer that owns the target's coordinates).
//
// Both are checked BEFORE an operation depends on them, which is the point of
// doing it here rather than where the copy is written: a sealer with no
// recipient, or with the fleet key's PRIVATE half in it, would otherwise be
// discovered at the moment the copy matters — while a datastore rollback or a
// host recovery is already under way. The probe seals an empty payload: it
// creates no object and leaves nothing behind.
func NewCopy(writer ObjectWriter, sealer Sealer, prefix string) (Copy, error) {
	switch {
	case writer == nil:
		return Copy{}, errors.New("the record copy needs an object writer: without one the record cannot leave the cluster it is protecting")
	case sealer == nil:
		return Copy{}, errors.New("the record copy needs a sealer: this copy goes to a target the customer's cluster does not control, so it never travels in the clear")
	}
	if _, err := sealer.Seal(nil); err != nil {
		return Copy{}, fmt.Errorf("this sealer cannot seal the record copy: %w", err)
	}
	return Copy{Writer: writer, Sealer: sealer, Prefix: copyPrefix(prefix)}, nil
}

// copyPrefix is the target scope as the copy's keys are built on: a directory,
// so it ends in "/".
//
// The prefixes that arrive here come from a target's `--prefix`, which is
// written without one ("clusters/prod-1"), and ObjectKey concatenates. Without
// this, a cluster's copy would land at
// "clusters/prod-1operations/<id>.json.enc" in the bucket root — beside no
// directory anyone lists, under no prefix anyone scoped a credential to. It is
// normalised rather than refused because the input is right and the
// concatenation is what is wrong.
func copyPrefix(prefix string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}

// ObjectKey is where one operation's copy lives.
func (c Copy) ObjectKey(opID string) string {
	return c.Prefix + "operations/" + opID + ".json.enc"
}

// OwnerKey is where one operation's recovery-owner object lives.
func (c Copy) OwnerKey(opID string) string {
	return c.Prefix + "recovery-owner/" + opID + ".json"
}

// Write puts an encrypted copy of the record on the target.
//
// The record is validated first, by the same guard the cluster write uses: this
// is one more place a credential must not land, and it is not the customer's
// cluster.
func (c Copy) Write(ctx context.Context, rec *Record) (string, error) {
	if c.Writer == nil || c.Sealer == nil {
		return "", errors.New("the record copy needs both an object writer and a sealer")
	}
	if !validOperationID(rec.OperationID) {
		return "", fmt.Errorf("operation id %q is not a usable identity, so it cannot name an object", rec.OperationID)
	}
	if err := rec.Validate(); err != nil {
		return "", err
	}
	plain, err := encodeRecord(rec)
	if err != nil {
		return "", err
	}
	sealed, err := c.Sealer.Seal(plain)
	if err != nil {
		return "", fmt.Errorf("encrypting the copy of operation %s: %w", rec.OperationID, err)
	}
	key := c.ObjectKey(rec.OperationID)
	if err := c.Writer.Put(ctx, key, sealed); err != nil {
		return key, fmt.Errorf("writing the copy of operation %s: %w", rec.OperationID, err)
	}
	return key, nil
}

// recoveryOwner is the object Claim creates. It carries no credentials: it is a
// name and a time, and it is read by a laptop that has nothing else left.
type recoveryOwner struct {
	OperationID string    `json:"operation_id"`
	Owner       string    `json:"owner"`
	ClaimedAt   time.Time `json:"claimed_at"`
	Note        string    `json:"note"`
}

// Claim takes exclusive ownership of an operation outside the cluster, by
// conditionally creating the recovery-owner object.
//
// This is the case the ConfigMap cannot cover: S6 and S11 recover a host whose
// API server is gone, so the cluster-side lock is gone with it. A conditional
// create is the whole mechanism — an unconditional PUT would let two operators
// both "win", which is why a target that cannot do it stops here instead
// (ErrNoConditionalWrites, F20) rather than falling back. Elapsed time never
// releases ownership; the object's existence is what does.
func (c Copy) Claim(ctx context.Context, opID, owner string, now time.Time) (string, error) {
	if c.Writer == nil {
		return "", errors.New("claiming an operation off-cluster needs an object writer")
	}
	if !validOperationID(opID) {
		return "", fmt.Errorf("operation id %q is not a usable identity, so it cannot name an object", opID)
	}
	key := c.OwnerKey(opID)
	body, err := json.Marshal(recoveryOwner{
		OperationID: opID,
		Owner:       owner,
		ClaimedAt:   now.UTC(),
		Note:        "conditional create; exists == this operator owns the recovery",
	})
	if err != nil {
		return key, err
	}
	created, err := c.Writer.PutIfAbsent(ctx, key, body)
	if err != nil {
		if errors.Is(err, ErrNoConditionalWrites) {
			return key, fmt.Errorf("%w: automatic recovery of operation %s on this target requires the documented single-operator rule to be acknowledged first (F20)", ErrNoConditionalWrites, opID)
		}
		return key, fmt.Errorf("claiming operation %s: %w", opID, err)
	}
	if !created {
		return key, fmt.Errorf("%w: %s is already held", ErrAlreadyClaimed, key)
	}
	return key, nil
}
