package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"kubenest.io/cli/pkg/operation"
)

// How ownership was taken, reported so the operation record can say which one
// happened.
const (
	// OwnershipConditional is a real conditional create: `If-None-Match: *`
	// created the object, so exactly one operator can hold it.
	OwnershipConditional = "conditional"
	// OwnershipAcknowledged is the F20 fallback: the target cannot create an
	// object conditionally, so the operator accepted the documented
	// single-operator rule and the ownership object was written
	// unconditionally. It excludes nobody, and the record says so.
	OwnershipAcknowledged = "acknowledged"
)

// acknowledgementNote is what an acknowledged ownership object carries where a
// conditional one carries its note, so a reader of the bucket can tell the two
// apart without the operation record.
const acknowledgementNote = "unconditional write; the operator acknowledged the single-operator rule because the target cannot create an object conditionally (F20)"

// recoveredOwner is the object an acknowledged claim writes. Its first four
// fields are the same ones a conditional claim writes, so one reader
// understands both.
type recoveredOwner struct {
	OperationID string    `json:"operation_id"`
	Owner       string    `json:"owner"`
	ClaimedAt   time.Time `json:"claimed_at"`
	Note        string    `json:"note"`
}

// Claim takes recovery ownership outside the cluster.
//
// OWNERSHIP IS TAKEN BEFORE THE FIRST DESTRUCTIVE STEP, because the cluster-side
// lock died with the host: on a lost single-server cluster, `kube-system`'s
// operation ConfigMap is gone, and two operators recovering the same cluster
// from two laptops would otherwise both "win" and both restore.
//
// The conditional create is the mechanism. When the target cannot do one, the
// caller must have the operator's explicit acknowledgement of the F20
// single-operator rule; without it this refuses rather than writing an object
// that excludes nobody. Elapsed time never releases ownership either way — the
// object's existence is what does.
func Claim(ctx context.Context, copy operation.Copy, opID, operator string, now time.Time, acknowledged bool) (key, mode string, err error) {
	if opID == "" {
		return "", "", errors.New("taking recovery ownership needs the operation id: it is the name the ownership object is written under")
	}
	if operator == "" {
		return "", "", errors.New("taking recovery ownership needs a name for the operator holding it: the object is read by a laptop that has nothing else left, and \"someone\" is not an answer")
	}
	key, claimErr := copy.Claim(ctx, opID, operator, now)
	if claimErr == nil {
		return key, OwnershipConditional, nil
	}
	if errors.Is(claimErr, operation.ErrAlreadyClaimed) {
		return key, "", claimErr
	}
	if !errors.Is(claimErr, operation.ErrNoConditionalWrites) {
		return key, "", claimErr
	}
	if !acknowledged {
		return key, "", fmt.Errorf("%w\n      This target cannot create an object conditionally, so a recovery here cannot prove it is the only one running. Recovery of a lost host is the one operation where that matters: two operators both restoring the same cluster is how the dead host's data comes back twice. Confirm the documented single-operator rule with --acknowledge-single-operator to record the acknowledgement and continue, or run the recovery against a target that supports conditional creation (probe P3 question 1)", claimErr)
	}
	body, err := json.Marshal(recoveredOwner{
		OperationID: opID,
		Owner:       operator,
		ClaimedAt:   now.UTC(),
		Note:        acknowledgementNote,
	})
	if err != nil {
		return key, "", err
	}
	if copy.Writer == nil {
		return key, "", errors.New("claiming a recovery off-cluster needs an object writer")
	}
	if err := copy.Writer.Put(ctx, key, body); err != nil {
		return key, "", fmt.Errorf("writing the acknowledged ownership object %s: %w", key, err)
	}
	return key, OwnershipAcknowledged, nil
}
