package operation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/s3"
)

// The tests below are the off-cluster half of T2.3 driven through the REAL
// pieces the CLI ships: pkg/s3's SigV4 client on the wire, and
// pkg/recoverykit's age sealer on the bytes. The fake writer in
// operation_test.go proves what the copy DOES with an object writer; these
// prove that the client and the sealer the CLI actually hands it behave — that
// `If-None-Match: *` is a conditional create a second operator loses, and that
// what lands in the bucket is the fleet key's to open and no one else's.
//
// What a green run does NOT prove: that a real S3 target honours
// If-None-Match: * (that is probe P3, and e2e/operation_record_test.go is the
// run on real hosts), or that a store which ignores the header would be
// detected — it answers 200 for a create that was not one, and telling that
// apart from a genuine create is exactly what P3 measures.

const (
	testBucket    = "kubenest-backups"
	testRegion    = "main"
	testAccessKey = "AKIAIOSFODNN7EXAMPLE"
	testSecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	// testScope is a target prefix as it is written on a command line: no
	// surrounding slashes, as `--prefix clusters/prod-1` is.
	testScope = "clusters/prod-1"
)

// testS3 is an S3 endpoint that honours `If-None-Match: *` the way a real one
// does: the second conditional create of an existing key is a 412, not an
// overwrite. It answers path-style, which is what pkg/s3 does for every
// endpoint that is not AWS.
type testS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	requests []testS3Request
}

type testS3Request struct {
	method      string
	path        string
	ifNoneMatch string
	auth        string
	contentType string
	body        []byte
}

func newTestS3(t *testing.T) *testS3 {
	t.Helper()
	return &testS3{objects: map[string][]byte{}}
}

func (s *testS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/"+testBucket+"/")

	s.mu.Lock()
	s.requests = append(s.requests, testS3Request{
		method:      r.Method,
		path:        r.URL.Path,
		ifNoneMatch: r.Header.Get("If-None-Match"),
		auth:        r.Header.Get("Authorization"),
		contentType: r.Header.Get("Content-Type"),
		body:        append([]byte(nil), body...),
	})
	if !ok || r.Method != http.MethodPut {
		s.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if _, exists := s.objects[key]; exists && r.Header.Get("If-None-Match") == "*" {
		s.mu.Unlock()
		w.WriteHeader(http.StatusPreconditionFailed)
		io.WriteString(w, `<Error><Code>PreconditionFailed</Code>`+
			`<Message>At least one of the pre-conditions you specified did not hold</Message></Error>`)
		return
	}
	s.objects[key] = append([]byte(nil), body...)
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// object returns a stored object, and whether it is there.
func (s *testS3) object(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.objects[key]
	return body, ok
}

func (s *testS3) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.objects))
	for key := range s.objects {
		out = append(out, key)
	}
	return out
}

func (s *testS3) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *testS3) lastRequest(t *testing.T) testS3Request {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		t.Fatal("the target saw no request at all")
	}
	return s.requests[len(s.requests)-1]
}

// target returns the httptest server and the client the CLI would build for it.
func (s *testS3) target(t *testing.T) (*httptest.Server, *s3.Client) {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	client, err := s3.New(s3.Config{
		Endpoint:        srv.URL,
		Bucket:          testBucket,
		Region:          testRegion,
		AccessKeyID:     testAccessKey,
		SecretAccessKey: testSecretKey,
	})
	if err != nil {
		t.Fatalf("building the SigV4 client for the test target: %v", err)
	}
	return srv, client
}

// mustFleetKey mints a fleet recovery key: the private half is what the operator
// keeps, the recipient is all a later install has.
func mustFleetKey(t *testing.T) *recoverykit.FleetKey {
	t.Helper()
	fleet, err := recoverykit.GenerateFleetKey()
	if err != nil {
		t.Fatalf("generating a fleet recovery key: %v", err)
	}
	return fleet
}

// copyOnARealTarget is the whole wiring: the SigV4 client the CLI builds for a
// target, and the age sealer to the fleet recipient.
func copyOnARealTarget(t *testing.T, s *testS3, recipient string) Copy {
	t.Helper()
	_, client := s.target(t)
	c, err := NewCopy(client, recoverykit.Sealer{Recipient: recipient}, testScope)
	if err != nil {
		t.Fatalf("building the record copy for the target: %v", err)
	}
	return c
}

// liveRecordFor acquires one operation and hands back the record the cluster
// actually holds — a real record, with a real operation id and ownership token.
func liveRecordFor(t *testing.T) *Record {
	t.Helper()
	k := newFakeKube(t)
	acquire(t, newStore(t, k, "ana@laptop"), testRequest("host-1"))
	return k.liveRecord(Name)
}

// TestTheCopyOnARealTargetIsTheFleetKeysToOpen: what the CLI writes to the
// bucket is the SEALED record, at the key a recovery looks for, and the fleet
// key is the only thing that opens it. The target's own credentials are not in
// it — the copy is not a place a credential travels.
func TestTheCopyOnARealTargetIsTheFleetKeysToOpen(t *testing.T) {
	ctx := context.Background()
	fleet := mustFleetKey(t)
	srv := newTestS3(t)
	c := copyOnARealTarget(t, srv, fleet.Recipient())
	rec := liveRecordFor(t)

	key, err := c.Write(ctx, rec)
	if err != nil {
		t.Fatalf("writing the copy: %v", err)
	}
	if want := testScope + "/operations/" + rec.OperationID + ".json.enc"; key != want {
		t.Fatalf("the copy landed at %q, want %q", key, want)
	}
	body, ok := srv.object(key)
	if !ok {
		t.Fatalf("the target holds %v: the copy never arrived", srv.keys())
	}
	plain, err := encodeRecord(rec)
	if err != nil {
		t.Fatalf("encoding the record: %v", err)
	}
	if bytes.Contains(body, plain) || bytes.Contains(body, []byte(rec.OperationID)) {
		t.Fatal("the copy is the plaintext record: it was not encrypted on the way out")
	}
	for _, credential := range []string{testAccessKey, testSecretKey} {
		if bytes.Contains(body, []byte(credential)) {
			t.Fatal("the copy carries the target's own credentials")
		}
	}
	opened, err := recoverykit.Open(fleet.SecretKeyString(), body)
	if err != nil {
		t.Fatalf("the fleet key does not open the copy: %v", err)
	}
	if !bytes.Equal(opened, plain) {
		t.Fatalf("the copy opens to %d bytes that are not the record (%d bytes)", len(opened), len(plain))
	}

	// Writing the copy is an ordinary PUT: no conditional header, because a
	// copy that had to be created at most once would be evidence pretending to
	// be a lock, and a second attempt to write EVIDENCE must be allowed.
	req := srv.lastRequest(t)
	if req.method != http.MethodPut || req.ifNoneMatch != "" {
		t.Fatalf("the copy went out as %s with If-None-Match %q", req.method, req.ifNoneMatch)
	}
	if !strings.HasPrefix(req.auth, "AWS4-HMAC-SHA256") {
		t.Fatalf("the copy was written unsigned (Authorization %q)", req.auth)
	}
}

// TestTheRecoveryOwnerClaimIsAConditionalCreateOnARealTarget: when the cluster's
// API is gone the ConfigMap lock is gone with it, and the only thing that
// excludes a second operator is the target refusing a conditional create it
// already has. The object's existence owns the recovery; elapsed time never
// releases it.
func TestTheRecoveryOwnerClaimIsAConditionalCreateOnARealTarget(t *testing.T) {
	ctx := context.Background()
	fleet := mustFleetKey(t)
	srv := newTestS3(t)
	c := copyOnARealTarget(t, srv, fleet.Recipient())
	rec := liveRecordFor(t)
	claimedAt := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

	ownerKey, err := c.Claim(ctx, rec.OperationID, "ana@laptop", claimedAt)
	if err != nil {
		t.Fatalf("claiming the recovery: %v", err)
	}
	if want := testScope + "/recovery-owner/" + rec.OperationID + ".json"; ownerKey != want {
		t.Fatalf("the claim landed at %q, want %q", ownerKey, want)
	}
	req := srv.lastRequest(t)
	if req.method != http.MethodPut || req.ifNoneMatch != "*" {
		t.Fatalf("the claim went out as %s with If-None-Match %q: without the conditional header two operators both win", req.method, req.ifNoneMatch)
	}
	body, ok := srv.object(ownerKey)
	if !ok {
		t.Fatalf("the target holds %v: the claim created nothing", srv.keys())
	}
	var owner recoveryOwner
	if err := json.Unmarshal(body, &owner); err != nil {
		t.Fatalf("the recovery-owner object is not readable JSON: %v", err)
	}
	if owner.OperationID != rec.OperationID || owner.Owner != "ana@laptop" || !owner.ClaimedAt.Equal(claimedAt) {
		t.Fatalf("the recovery-owner object is %+v", owner)
	}
	// A name and a time: nothing a credential can sit in, and nothing to
	// decrypt either — this object is read by a laptop that has nothing else.
	if bad := credentialFieldsIn(body); len(bad) != 0 {
		t.Fatalf("the recovery-owner object has credential-shaped fields: %v", bad)
	}
	for _, credential := range []string{testAccessKey, testSecretKey} {
		if bytes.Contains(body, []byte(credential)) {
			t.Fatal("the recovery-owner object carries the target's credentials")
		}
	}

	// A second operator is refused by the target, and the refusal is the one
	// the caller can act on.
	if _, err := c.Claim(ctx, rec.OperationID, "bo@other-laptop", claimedAt.Add(time.Hour)); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("a second claim returned %v, want ErrAlreadyClaimed", err)
	}
	after, _ := srv.object(ownerKey)
	if !bytes.Equal(after, body) {
		t.Fatal("the refused claim overwrote the owner object: asking twice took the recovery")
	}
	// The copy and the claim are separate objects, and taking the lock wrote no
	// evidence: the copy is written by the operation, not by the claim.
	if _, ok := srv.object(c.ObjectKey(rec.OperationID)); ok {
		t.Fatal("the claim wrote the record copy")
	}
}

// TestARecordCopyRefusesASeamThatCannotWork: the two pieces are checked before
// an operation depends on them, because the moment the copy matters is the
// moment a datastore rollback or a host recovery is already under way. The
// fleet key's PRIVATE half in the recipient position is refused here — this
// copy is written to a store the customer's cluster does not control.
func TestARecordCopyRefusesASeamThatCannotWork(t *testing.T) {
	ctx := context.Background()
	fleet := mustFleetKey(t)
	srv := newTestS3(t)
	_, client := srv.target(t)

	if _, err := NewCopy(nil, recoverykit.Sealer{Recipient: fleet.Recipient()}, testScope); err == nil {
		t.Fatal("a copy was built with no object writer: the record could not leave the cluster")
	}
	if _, err := NewCopy(client, nil, testScope); err == nil {
		t.Fatal("a copy was built with no sealer: it would travel in the clear")
	}
	for _, bad := range []string{
		fleet.SecretKeyString(), // the private half, not a recipient
		"",                      // nothing to seal to
		"not-an-age-recipient",
	} {
		if _, err := NewCopy(client, recoverykit.Sealer{Recipient: bad}, testScope); err == nil {
			t.Fatalf("a copy was built with %q in the recipient position", bad)
		}
	}
	// Checking them is not a write: no probe object, no request.
	if n := srv.count(); n != 0 {
		t.Fatalf("building the copy made %d request(s) to the target", n)
	}

	// The prefix is the target scope AS the copy's keys are built on: a
	// `--prefix clusters/prod-1` must not become "clusters/prod-1operations/…"
	// in the bucket root.
	c, err := NewCopy(client, recoverykit.Sealer{Recipient: fleet.Recipient()}, "clusters/prod-1")
	if err != nil {
		t.Fatalf("building the copy: %v", err)
	}
	if got, want := c.ObjectKey("a1b2c3d4e5f60718"), "clusters/prod-1/operations/a1b2c3d4e5f60718.json.enc"; got != want {
		t.Fatalf("the copy's key is %q, want %q", got, want)
	}
	if got, want := c.OwnerKey("a1b2c3d4e5f60718"), "clusters/prod-1/recovery-owner/a1b2c3d4e5f60718.json"; got != want {
		t.Fatalf("the claim's key is %q, want %q", got, want)
	}
	// It is the real client and the real sealer that were wired in.
	if _, ok := c.Writer.(*s3.Client); !ok {
		t.Fatalf("the copy's writer is %T, not the SigV4 client", c.Writer)
	}
	if _, err := c.Write(ctx, liveRecordFor(t)); err != nil {
		t.Fatalf("writing through the wired copy: %v", err)
	}
}
