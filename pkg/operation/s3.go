package operation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ObjectWriter is the slice of an S3 client this package needs: one
// unconditional PUT for the encrypted copy, one conditional PUT for the
// recovery-owner object.
//
// It is declared here rather than imported because the client does not exist
// yet: the SigV4 PUT belongs in kubenest-cli/pkg/s3 (T4.6), and this package
// must not grow a second one. PutIfAbsent is `If-None-Match: *` — a real
// conditional create — and its bool reports whether THIS call created the
// object.
type ObjectWriter interface {
	Put(ctx context.Context, key string, body []byte) error
	PutIfAbsent(ctx context.Context, key string, body []byte) (created bool, err error)
}

// Sealer encrypts the record copy. The fleet recovery key (T4.6) owns the only
// key that can open it, and this package deliberately never sees one: the copy
// is written to a target the customer's cluster does not control.
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
type Copy struct {
	Writer ObjectWriter
	Sealer Sealer
	// Prefix is the key prefix copies and claims live under.
	Prefix string
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
