package backup

import (
	"bytes"
	"testing"

	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/s3"
)

// copyTarget is the target fixture with the cluster prefix a `--prefix` gives
// it: written without a trailing slash, as an operator writes it.
func copyTarget() Target {
	t := testTarget()
	t.Prefix = "clusters/prod-1"
	return t
}

// TestTheTargetBuildsTheRecordCopyFromItsOwnCoordinatesAndCredential: the
// off-cluster copy of an operation's record is written to the SAME target and
// under the SAME cluster prefix the backups and the kits use — that is the
// credential the CLI holds and the store a recovery reaches when the cluster's
// API server is gone — so the wiring is the target's own coordinates, its
// SigV4 client, and the fleet recipient.
func TestTheTargetBuildsTheRecordCopyFromItsOwnCoordinatesAndCredential(t *testing.T) {
	fleet, err := recoverykit.GenerateFleetKey()
	if err != nil {
		t.Fatalf("generating a fleet recovery key: %v", err)
	}

	c, err := copyTarget().OperationCopy(fleet.Recipient())
	if err != nil {
		t.Fatalf("building the record copy for the target: %v", err)
	}
	if _, ok := c.Writer.(*s3.Client); !ok {
		t.Fatalf("the copy's writer is %T, not the target's SigV4 client", c.Writer)
	}
	// The keys sit beside the kits', under the target's prefix rather than at
	// the bucket root: `--prefix clusters/prod-1` is written without a trailing
	// slash, and the key is a concatenation.
	if got, want := c.ObjectKey("a1b2c3d4e5f60718"), "clusters/prod-1/operations/a1b2c3d4e5f60718.json.enc"; got != want {
		t.Fatalf("the copy's key is %q, want %q", got, want)
	}
	if got, want := c.OwnerKey("a1b2c3d4e5f60718"), "clusters/prod-1/recovery-owner/a1b2c3d4e5f60718.json"; got != want {
		t.Fatalf("the claim's key is %q, want %q", got, want)
	}
	// The sealer is the fleet RECIPIENT: the copy is written to a store the
	// customer's cluster does not control, so the fleet key is the only thing
	// that opens it.
	sealed, err := c.Sealer.Seal([]byte(`{"operation_id":"a1b2c3d4e5f60718"}`))
	if err != nil {
		t.Fatalf("sealing the record copy: %v", err)
	}
	opened, err := recoverykit.Open(fleet.SecretKeyString(), sealed)
	if err != nil {
		t.Fatalf("what the target built does not open with the fleet key: %v", err)
	}
	if !bytes.Contains(opened, []byte("a1b2c3d4e5f60718")) {
		t.Fatal("the sealed copy does not carry the record it sealed")
	}
}

// TestTheTargetRefusesToBuildACopyFromThePrivateHalf: the recipient is the
// public half of the fleet key. A private key here would travel to a component
// whose whole job is to write the record outside the cluster it protects, and
// it is refused before an operation starts rather than at the moment the copy
// matters.
func TestTheTargetRefusesToBuildACopyFromThePrivateHalf(t *testing.T) {
	fleet, err := recoverykit.GenerateFleetKey()
	if err != nil {
		t.Fatalf("generating a fleet recovery key: %v", err)
	}
	if _, err := copyTarget().OperationCopy(fleet.SecretKeyString()); err == nil {
		t.Fatal("a target built the record copy from the fleet key's private half")
	}
	if _, err := copyTarget().OperationCopy(""); err == nil {
		t.Fatal("a target built the record copy with no recipient at all")
	}

	// A target that is not usable as a target is not usable as a copy either:
	// the S3 client is built from the same coordinates and credentials.
	broken := copyTarget()
	broken.SecretAccessKey = ""
	if _, err := broken.OperationCopy(fleet.Recipient()); err == nil {
		t.Fatal("a target with no credentials built the record copy")
	}
}
