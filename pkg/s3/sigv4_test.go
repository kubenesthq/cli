package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kubenest.io/cli/pkg/operation"
)

const (
	testAccessKey   = "AKIAIOSFODNN7EXAMPLE"
	testSecretKey   = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	testRegion      = "us-east-1"
	testBucket      = "recovery-kit"
	emptyBodySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// *Client is the ObjectWriter pkg/operation declares for the off-cluster
// record copy: Put is the unconditional write and PutIfAbsent is the real
// conditional create (If-None-Match: *).
var _ operation.ObjectWriter = (*Client)(nil)

// signatureRE matches the hex signature SigV4 puts in the Authorization header.
var signatureRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// recordedRequest is one request a test server saw.
type recordedRequest struct {
	method      string
	path        string
	escapedPath string
	rawQuery    string
	query       url.Values
	host        string
	ifNoneMatch string
	sha         string
	amzDate     string
	auth        string
}

// recorder is an http.Handler that records every request before delegating.
// It makes the assertions about what actually went on the wire (path style,
// conditional header, query parameters) possible and non-vacuous.
type recorder struct {
	mu       sync.Mutex
	requests []recordedRequest
	handler  func(w http.ResponseWriter, r *http.Request)
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec.mu.Lock()
	rec.requests = append(rec.requests, recordedRequest{
		method:      r.Method,
		path:        r.URL.Path,
		escapedPath: r.URL.EscapedPath(),
		rawQuery:    r.URL.RawQuery,
		query:       r.URL.Query(),
		host:        r.Host,
		ifNoneMatch: r.Header.Get("If-None-Match"),
		sha:         r.Header.Get("X-Amz-Content-Sha256"),
		amzDate:     r.Header.Get("X-Amz-Date"),
		auth:        r.Header.Get("Authorization"),
	})
	rec.mu.Unlock()
	rec.handler(w, r)
}

func (rec *recorder) last(t *testing.T) recordedRequest {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.requests) == 0 {
		t.Fatal("no request reached the server")
	}
	return rec.requests[len(rec.requests)-1]
}

func (rec *recorder) all() []recordedRequest {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]recordedRequest(nil), rec.requests...)
}

func (rec *recorder) count() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return len(rec.requests)
}

func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

func mustClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hasSignedHeader(signed, name string) bool {
	return slices.Contains(strings.Split(signed, ";"), name)
}

// parseAuthorization splits and shape-checks an Authorization header.
func parseAuthorization(t *testing.T, header string) (credential, signedHeaders, signature string) {
	t.Helper()
	const prefix = "AWS4-HMAC-SHA256 "
	if !strings.HasPrefix(header, prefix) {
		t.Fatalf("Authorization %q does not start with %q", header, prefix)
	}
	fields := strings.Split(strings.TrimPrefix(header, prefix), ",")
	if len(fields) != 3 {
		t.Fatalf("Authorization %q has %d comma-separated fields, want 3", header, len(fields))
	}
	var ok bool
	if credential, ok = strings.CutPrefix(fields[0], "Credential="); !ok {
		t.Fatalf("Authorization %q: first field is not Credential=", header)
	}
	if signedHeaders, ok = strings.CutPrefix(fields[1], "SignedHeaders="); !ok {
		t.Fatalf("Authorization %q: second field is not SignedHeaders=", header)
	}
	if signature, ok = strings.CutPrefix(fields[2], "Signature="); !ok {
		t.Fatalf("Authorization %q: third field is not Signature=", header)
	}
	if !signatureRE.MatchString(signature) {
		t.Fatalf("Authorization %q: signature %q is not 64 lowercase hex digits", header, signature)
	}
	return credential, signedHeaders, signature
}

// TestConditionalCreateIsIfNoneMatch proves PutIfAbsent is a real conditional
// create: the header is on the wire, the signature covers it, and the three
// outcomes (created, already-there, refused) are told apart.
func TestConditionalCreateIsIfNoneMatch(t *testing.T) {
	const key = "kits/2026-09-25.tar"
	body := []byte("recovery-kit-bytes")
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

	var status atomic.Int32
	rec := &recorder{handler: func(w http.ResponseWriter, r *http.Request) {
		switch status.Load() {
		case http.StatusOK:
			w.WriteHeader(http.StatusOK)
		case http.StatusPreconditionFailed:
			w.WriteHeader(http.StatusPreconditionFailed)
			io.WriteString(w, `<Error><Code>PreconditionFailed</Code><Message>At least one of the pre-conditions you specified did not hold</Message></Error>`)
		case http.StatusForbidden:
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	c := mustClient(t, Config{
		Endpoint:        srv.URL,
		Bucket:          testBucket,
		Region:          testRegion,
		AccessKeyID:     testAccessKey,
		SecretAccessKey: testSecretKey,
		Now:             fixedClock(now),
	})
	ctx := context.Background()

	// 1. an object that does not exist yet is created.
	status.Store(http.StatusOK)
	created, err := c.PutIfAbsent(ctx, key, body)
	if err != nil {
		t.Fatalf("PutIfAbsent on a fresh key: %v", err)
	}
	if !created {
		t.Fatalf("PutIfAbsent on a fresh key: created = false, want true")
	}
	req := rec.last(t)
	if req.method != http.MethodPut {
		t.Errorf("method = %s, want PUT", req.method)
	}
	if want := "/" + testBucket + "/" + key; req.path != want {
		t.Errorf("request path = %q, want %q (non-AWS endpoints are path style)", req.path, want)
	}
	if want := strings.TrimPrefix(srv.URL, "http://"); req.host != want {
		t.Errorf("request host = %q, want %q (the bucket must not be in the host)", req.host, want)
	}
	if req.ifNoneMatch != "*" {
		t.Errorf("If-None-Match = %q, want %q", req.ifNoneMatch, "*")
	}
	if req.sha != sha256hex(body) {
		t.Errorf("X-Amz-Content-Sha256 = %q, want %q", req.sha, sha256hex(body))
	}
	if req.amzDate != "20260925T120000Z" {
		t.Errorf("X-Amz-Date = %q, want %q", req.amzDate, "20260925T120000Z")
	}
	credential, signed, signature := parseAuthorization(t, req.auth)
	if want := testAccessKey + "/20260925/" + testRegion + "/s3/aws4_request"; credential != want {
		t.Errorf("Credential = %q, want %q", credential, want)
	}
	for _, h := range []string{"host", "if-none-match", "x-amz-content-sha256", "x-amz-date"} {
		if !hasSignedHeader(signed, h) {
			t.Errorf("SignedHeaders %q does not cover %q", signed, h)
		}
	}

	// 2. a different body must sign differently, or the signature is a constant.
	created, err = c.PutIfAbsent(ctx, key, []byte("another kit, other bytes"))
	if err != nil || !created {
		t.Fatalf("PutIfAbsent on a fresh key: created = %v, err = %v", created, err)
	}
	if other := rec.last(t).auth; other == req.auth {
		t.Errorf("the signature did not change with the body: %s", signature)
	}

	// 3. 412 means somebody else created it: not created, and not an error.
	status.Store(http.StatusPreconditionFailed)
	created, err = c.PutIfAbsent(ctx, key, body)
	if err != nil {
		t.Errorf("412 must not be an error, got %v", err)
	}
	if created {
		t.Errorf("412 must report created = false")
	}

	// 4. anything else non-2xx is an error, mapped for the caller.
	status.Store(http.StatusForbidden)
	created, err = c.PutIfAbsent(ctx, key, body)
	if created {
		t.Errorf("403 must report created = false")
	}
	if err == nil {
		t.Fatal("403 must be an error")
	}
	if !errors.Is(err, ErrAccessDenied) {
		t.Errorf("errors.Is(err, ErrAccessDenied) = false for %v", err)
	}
	var s3err *Error
	if !errors.As(err, &s3err) {
		t.Fatalf("errors.As(err, *Error) = false for %v", err)
	}
	if s3err.Op != "PutObject" || s3err.Key != key || s3err.StatusCode != http.StatusForbidden || s3err.Code != "AccessDenied" {
		t.Errorf("Error = %+v, want Op PutObject, Key %q, StatusCode 403, Code AccessDenied", s3err, key)
	}
	if !strings.Contains(err.Error(), "PutObject") {
		t.Errorf("Error() = %q, want it to name the operation", err.Error())
	}

	// 5. the unconditional Put must not send the conditional header.
	status.Store(http.StatusOK)
	if err := c.Put(ctx, key, body); err != nil {
		t.Fatalf("Put: %v", err)
	}
	req = rec.last(t)
	if req.ifNoneMatch != "" {
		t.Errorf("Put sent If-None-Match: %q", req.ifNoneMatch)
	}
	if req.method != http.MethodPut || req.sha != sha256hex(body) {
		t.Errorf("Put: method = %s, X-Amz-Content-Sha256 = %q", req.method, req.sha)
	}
}

// TestParsesBucketProtectionAndVersioning checks both the parse and the fact
// that the client asked: a client that never sent ?encryption/?versioning
// would fail the request-parameter assertion, not just the value assertions.
func TestParsesBucketProtectionAndVersioning(t *testing.T) {
	var mode atomic.Value
	mode.Store("")

	rec := &recorder{handler: func(w http.ResponseWriter, r *http.Request) {
		mode, _ := mode.Load().(string)
		switch {
		case r.URL.Query().Has("encryption"):
			switch mode {
			case "enc-present":
				w.WriteHeader(http.StatusOK)
				io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><ServerSideEncryptionConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault><BucketKeyEnabled>true</BucketKeyEnabled></Rule></ServerSideEncryptionConfiguration>`)
			case "enc-absent":
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `<Error><Code>ServerSideEncryptionConfigurationNotFoundError</Code><Message>The server side encryption configuration was not found</Message><BucketName>recovery-kit</BucketName></Error>`)
			default:
				w.WriteHeader(http.StatusInternalServerError)
			}
		case r.URL.Query().Has("versioning"):
			switch mode {
			case "ver-enabled":
				w.WriteHeader(http.StatusOK)
				io.WriteString(w, `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`)
			case "ver-empty":
				w.WriteHeader(http.StatusOK)
				io.WriteString(w, `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"/>`)
			case "ver-denied":
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
			default:
				w.WriteHeader(http.StatusInternalServerError)
			}
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	c := mustClient(t, Config{
		Endpoint:        srv.URL,
		Bucket:          testBucket,
		Region:          testRegion,
		AccessKeyID:     testAccessKey,
		SecretAccessKey: testSecretKey,
	})
	ctx := context.Background()

	mode.Store("enc-present")
	prot, err := c.BucketEncryption(ctx)
	if err != nil {
		t.Fatalf("BucketEncryption: %v", err)
	}
	if prot.Algorithm != "AES256" {
		t.Errorf("Algorithm = %q, want AES256", prot.Algorithm)
	}

	mode.Store("enc-absent")
	prot, err = c.BucketEncryption(ctx)
	if err != nil {
		t.Errorf("a bucket with no default encryption must not be an error: %v", err)
	}
	if prot.Algorithm != "" {
		t.Errorf("Algorithm = %q, want empty", prot.Algorithm)
	}

	mode.Store("ver-enabled")
	ver, err := c.BucketVersioning(ctx)
	if err != nil {
		t.Fatalf("BucketVersioning: %v", err)
	}
	if ver.Status != "Enabled" {
		t.Errorf("Status = %q, want Enabled", ver.Status)
	}

	mode.Store("ver-empty")
	ver, err = c.BucketVersioning(ctx)
	if err != nil {
		t.Errorf("a never-configured bucket must not be an error: %v", err)
	}
	if ver.Status != "" {
		t.Errorf("Status = %q, want empty", ver.Status)
	}

	mode.Store("ver-denied")
	ver, err = c.BucketVersioning(ctx)
	if err == nil {
		t.Fatal("403 on ?versioning must be an error")
	}
	if ver.Status != "" {
		t.Errorf("Status = %q on error, want empty", ver.Status)
	}
	if !errors.Is(err, ErrAccessDenied) {
		t.Errorf("errors.Is(err, ErrAccessDenied) = false for %v", err)
	}
	var s3err *Error
	if !errors.As(err, &s3err) || s3err.Op != "GetBucketVersioning" || s3err.Key != "" {
		t.Errorf("Error = %+v, want Op GetBucketVersioning on a bucket-level call", s3err)
	}

	// Every request must have been a GET of the bucket itself, and the two
	// configurations must have been asked for by name.
	var asked []string
	for _, r := range rec.all() {
		if r.method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.method)
		}
		if want := "/" + testBucket; r.path != want {
			t.Errorf("request path = %q, want %q", r.path, want)
		}
		switch {
		case r.query.Has("encryption"):
			asked = append(asked, "encryption")
		case r.query.Has("versioning"):
			asked = append(asked, "versioning")
		default:
			t.Errorf("request %s carried neither ?encryption nor ?versioning", r.rawQuery)
		}
	}
	want := []string{"encryption", "encryption", "versioning", "versioning", "versioning"}
	if !slices.Equal(asked, want) {
		t.Errorf("the client asked for %v, want %v", asked, want)
	}
}

// TestSigV4MatchesThePublishedAWSExample pins the signature against AWS's own
// "GET Object" vector, so the test fails if the canonical request, the derived
// key or the header canonicalization drifts by a byte.
func TestSigV4MatchesThePublishedAWSExample(t *testing.T) {
	const (
		bucket = "examplebucket"
		key    = "test.txt"
		want   = "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request," +
			"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date," +
			"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	)
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

	c := mustClient(t, Config{
		Endpoint:        "s3.amazonaws.com",
		Bucket:          bucket,
		Region:          testRegion,
		AccessKeyID:     testAccessKey,
		SecretAccessKey: testSecretKey,
		Now:             fixedClock(now),
	})

	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	req.Header.Set("Range", "bytes=0-9")
	req.Header.Set("X-Amz-Date", "20130524T000000Z")

	if got := c.sign(req, emptyBodySHA256, now); got != want {
		t.Errorf("Authorization =\n %s\nwant\n %s", got, want)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20130524T000000Z" {
		t.Errorf("X-Amz-Date = %q, want 20130524T000000Z", got)
	}
	if got := req.Header.Get("X-Amz-Content-Sha256"); got != emptyBodySHA256 {
		t.Errorf("X-Amz-Content-Sha256 = %q, want %q", got, emptyBodySHA256)
	}

	// A key that needs escaping is encoded exactly once - the same bytes on
	// the wire and in the canonical request. The expectation below comes from
	// an independent SigV4 implementation, the one that reproduces the
	// published vector above.
	escKey, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/kits/2026%20a%2Bb.tar", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	const wantEscapedKeyAuth = "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request," +
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date," +
		"Signature=a87f2e3b6ed5848303550e2046946b35055a2f9768c8ca094056c64ceebee435"
	if got := c.sign(escKey, emptyBodySHA256, now); got != wantEscapedKeyAuth {
		t.Errorf("Authorization for an escaped key =\n %s\nwant\n %s", got, wantEscapedKeyAuth)
	}
	if got := escKey.URL.EscapedPath(); got != "/kits/2026%20a%2Bb.tar" {
		t.Errorf("request path = %q, want the encoding the signature covers", got)
	}
	if got := canonicalURI("/kits/2026/a b+c.tar"); got != "/kits/2026/a%20b%2Bc.tar" {
		t.Errorf("canonicalURI = %q, want each segment escaped and / preserved", got)
	}

	// Header values are trimmed and their runs of whitespace collapsed, and the
	// signed names are sorted.
	note, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/kits/2026%20a%2Bb.tar", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	note.Header.Set("X-Amz-Meta-Note", "  padded   value  ")
	note.Header.Set("X-Amz-Content-Sha256", emptyBodySHA256)
	note.Header.Set("X-Amz-Date", "20130524T000000Z")
	block, names := canonicalHeaders(note)
	if !strings.Contains(block, "x-amz-meta-note:padded value\n") {
		t.Errorf("canonical headers = %q, want the note's whitespace collapsed", block)
	}
	if want := "host;x-amz-content-sha256;x-amz-date;x-amz-meta-note"; names != want {
		t.Errorf("signed headers = %q, want %q", names, want)
	}

	// AWS endpoints are virtual-hosted, everything else is path style.
	if got := c.urlFor(key, nil); got != "https://examplebucket.s3.amazonaws.com/test.txt" {
		t.Errorf("urlFor on AWS = %q", got)
	}
	if got := c.urlFor("", nil); got != "https://examplebucket.s3.amazonaws.com/" {
		t.Errorf("bucket-level urlFor on AWS = %q", got)
	}

	other := mustClient(t, Config{
		Endpoint:        "minio.internal:9000",
		Bucket:          bucket,
		Region:          "us-east-1",
		AccessKeyID:     testAccessKey,
		SecretAccessKey: testSecretKey,
	})
	if got := other.urlFor(key, nil); got != "https://minio.internal:9000/examplebucket/test.txt" {
		t.Errorf("urlFor on a non-AWS endpoint = %q, want the bucket in the path", got)
	}
	if got := other.urlFor("", url.Values{"list-type": {"2"}, "prefix": {"kits/"}}); got != "https://minio.internal:9000/examplebucket?list-type=2&prefix=kits%2F" {
		t.Errorf("bucket-level urlFor on a non-AWS endpoint = %q", got)
	}
	// Each key segment is RFC 3986-encoded; the "/" separators are not.
	if got := other.urlFor("kits/2026/a b+c.tar", nil); got != "https://minio.internal:9000/examplebucket/kits/2026/a%20b%2Bc.tar" {
		t.Errorf("urlFor with an escapable key = %q", got)
	}

	// The canonical query is part of the canonical request, so it must be
	// sorted by name then value and stable: url.Values is a map, and an
	// unsorted implementation would hand each call a different permutation.
	params := url.Values{
		"list-type":          {"2"},
		"prefix":             {"kits/"},
		"delimiter":          {"/"},
		"continuation-token": {"a b+c"},
		"max-keys":           {"1000"},
		"tag":                {"b", "a"},
	}
	const wantQuery = "continuation-token=a%20b%2Bc&delimiter=%2F&list-type=2&max-keys=1000&prefix=kits%2F&tag=a&tag=b"
	for i := 0; i < 64; i++ {
		if got := canonicalQuery(params); got != wantQuery {
			t.Fatalf("canonicalQuery (call %d) = %q, want %q", i, got, wantQuery)
		}
	}
	if got := other.urlFor("", params); got != "https://minio.internal:9000/examplebucket?"+wantQuery {
		t.Errorf("urlFor with query = %q, want the canonical query %q", got, wantQuery)
	}
}

// TestListIsOnePage checks the keys and the truncation flag are read, and that
// a truncated page triggers no follow-up request.
func TestListIsOnePage(t *testing.T) {
	rec := &recorder{handler: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `<ListBucketResult><Name>recovery-kit</Name><Prefix>kits/</Prefix><IsTruncated>true</IsTruncated><Contents><Key>kits/a.tar</Key><Size>10</Size></Contents><Contents><Key>kits/b.tar</Key><Size>20</Size></Contents><NextContinuationToken>tok</NextContinuationToken></ListBucketResult>`)
	}}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	c := mustClient(t, Config{
		Endpoint:        srv.URL,
		Bucket:          testBucket,
		Region:          testRegion,
		AccessKeyID:     testAccessKey,
		SecretAccessKey: testSecretKey,
	})

	keys, truncated, err := c.List(context.Background(), "kits/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !truncated {
		t.Errorf("truncated = false, want true (IsTruncated is true)")
	}
	if want := []string{"kits/a.tar", "kits/b.tar"}; !slices.Equal(keys, want) {
		t.Errorf("keys = %v, want %v", keys, want)
	}
	if n := rec.count(); n != 1 {
		t.Errorf("List made %d requests, want 1: it must return one page", n)
	}
	req := rec.last(t)
	if req.method != http.MethodGet {
		t.Errorf("method = %s, want GET", req.method)
	}
	if want := "/" + testBucket; req.path != want {
		t.Errorf("request path = %q, want %q", req.path, want)
	}
	if got := req.query.Get("list-type"); got != "2" {
		t.Errorf("list-type = %q, want 2 (query was %q)", got, req.rawQuery)
	}
	if got := req.query.Get("prefix"); got != "kits/" {
		t.Errorf("prefix = %q, want %q", got, "kits/")
	}
	_, signed, _ := parseAuthorization(t, req.auth)
	if !hasSignedHeader(signed, "host") {
		t.Errorf("SignedHeaders %q does not cover host", signed)
	}
}

// TestGetHeadAndErrorMapping covers the read paths the recovery flow uses and
// the status-based mapping of a body-less refusal.
func TestGetHeadAndErrorMapping(t *testing.T) {
	const (
		key  = "kits/daily a+b.tar"
		gone = "kits/missing.tar"
	)
	payload := []byte("kit-bytes")

	rec := &recorder{handler: func(w http.ResponseWriter, r *http.Request) {
		missing := strings.HasSuffix(r.URL.Path, "/missing.tar")
		switch {
		case missing && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`)
		case missing:
			// A HEAD refusal has no body at all.
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodHead:
			w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
			w.Header().Set("Content-Length", "42")
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
			w.Write(payload)
		}
	}}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	c := mustClient(t, Config{
		Endpoint:        srv.URL,
		Bucket:          testBucket,
		Region:          testRegion,
		AccessKeyID:     testAccessKey,
		SecretAccessKey: testSecretKey,
	})
	ctx := context.Background()

	got, err := c.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("Get body = %q, want %q", got, payload)
	}
	req := rec.last(t)
	if want := "/" + testBucket + "/" + key; req.path != want {
		t.Errorf("request path = %q, want %q", req.path, want)
	}
	if want := "/" + testBucket + "/kits/daily%20a%2Bb.tar"; req.escapedPath != want {
		t.Errorf("escaped request path = %q, want %q (space is %%20, + is %%2B)", req.escapedPath, want)
	}
	if sha := req.sha; sha != emptyBodySHA256 {
		t.Errorf("X-Amz-Content-Sha256 = %q on a body-less GET, want the empty-body hash", sha)
	}

	got, err = c.Get(ctx, gone)
	if err == nil {
		t.Fatal("Get of a missing key must be an error")
	}
	if got != nil {
		t.Errorf("Get of a missing key returned %q", got)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("errors.Is(err, ErrNotFound) = false for %v", err)
	}
	var s3err *Error
	if !errors.As(err, &s3err) || s3err.Op != "GetObject" || s3err.Code != "NoSuchKey" || s3err.StatusCode != http.StatusNotFound {
		t.Errorf("Error = %+v, want Op GetObject, Code NoSuchKey, StatusCode 404", s3err)
	}

	obj, err := c.Head(ctx, key)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if obj.Size != 42 || obj.ETag != `"d41d8cd98f00b204e9800998ecf8427e"` {
		t.Errorf("Head = %+v, want Size 42 and the ETag header", obj)
	}

	if _, err := c.Head(ctx, gone); !errors.Is(err, ErrNotFound) {
		t.Errorf("Head of a missing key: errors.Is(err, ErrNotFound) = false for %v", err)
	}
}

// TestHeadBucketMapsOutcomes checks the bucket-level HEAD k3s's datastore
// snapshots depend on: 2xx is nil, and a refusal is the mapped *Error the
// caller discriminates on by status, because a HEAD carries no body to
// classify it with.
func TestHeadBucketMapsOutcomes(t *testing.T) {
	var status atomic.Int64
	status.Store(http.StatusOK)

	rec := &recorder{handler: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	c := mustClient(t, Config{
		Endpoint:        srv.URL,
		Bucket:          testBucket,
		Region:          testRegion,
		AccessKeyID:     testAccessKey,
		SecretAccessKey: testSecretKey,
	})
	ctx := context.Background()

	if err := c.HeadBucket(ctx); err != nil {
		t.Fatalf("HeadBucket of an existing bucket: %v", err)
	}
	req := rec.last(t)
	if req.method != http.MethodHead {
		t.Errorf("method = %s, want HEAD", req.method)
	}
	// The bucket itself, not an object under it: k3s asks about the bucket.
	if want := "/" + testBucket; req.path != want {
		t.Errorf("path = %q, want %q", req.path, want)
	}

	// A refusal: 403 must map to ErrAccessDenied, with the operation named so
	// the policy the operator has to change is unambiguous.
	status.Store(http.StatusForbidden)
	err := c.HeadBucket(ctx)
	if err == nil {
		t.Fatal("403 on HeadBucket must be an error")
	}
	if !errors.Is(err, ErrAccessDenied) {
		t.Errorf("errors.Is(err, ErrAccessDenied) = false for %v", err)
	}
	var s3err *Error
	if !errors.As(err, &s3err) || s3err.Op != "HeadBucket" || s3err.StatusCode != http.StatusForbidden {
		t.Errorf("Error = %+v, want Op HeadBucket, StatusCode 403", s3err)
	}

	// Anything else non-2xx is still the package's error type rather than a
	// nil return that would read as "the bucket is there".
	status.Store(http.StatusNotFound)
	if err := c.HeadBucket(ctx); err == nil {
		t.Error("404 on HeadBucket must be an error, not a silent pass")
	}
}

// TestNewValidatesConfig checks the refusals happen before any request is
// attempted.
func TestNewValidatesConfig(t *testing.T) {
	valid := Config{
		Endpoint:        "https://minio.internal:9000",
		Bucket:          testBucket,
		Region:          testRegion,
		AccessKeyID:     testAccessKey,
		SecretAccessKey: testSecretKey,
	}
	if _, err := New(valid); err != nil {
		t.Fatalf("New(valid): %v", err)
	}

	tests := []struct {
		name string
		cfg  Config
		want string // substring naming the offending field
	}{
		{"no endpoint", Config{Bucket: testBucket, Region: testRegion, AccessKeyID: "k", SecretAccessKey: "s"}, "endpoint"},
		{"blank endpoint", Config{Endpoint: "   ", Bucket: testBucket, Region: testRegion, AccessKeyID: "k", SecretAccessKey: "s"}, "endpoint"},
		{"no bucket", Config{Endpoint: "s3.amazonaws.com", Region: testRegion, AccessKeyID: "k", SecretAccessKey: "s"}, "bucket"},
		{"no region", Config{Endpoint: "s3.amazonaws.com", Bucket: testBucket, AccessKeyID: "k", SecretAccessKey: "s"}, "region"},
		{"no access key", Config{Endpoint: "s3.amazonaws.com", Bucket: testBucket, Region: testRegion, SecretAccessKey: "s"}, "access key"},
		{"no secret", Config{Endpoint: "s3.amazonaws.com", Bucket: testBucket, Region: testRegion, AccessKeyID: "k"}, "secret"},
		{"no host", Config{Endpoint: "https://", Bucket: testBucket, Region: testRegion, AccessKeyID: "k", SecretAccessKey: "s"}, "endpoint"},
		{"bad scheme", Config{Endpoint: "ftp://minio.internal", Bucket: testBucket, Region: testRegion, AccessKeyID: "k", SecretAccessKey: "s"}, "endpoint"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(tt.cfg)
			if err == nil {
				t.Fatalf("New(%+v) succeeded (client %+v), want an error", tt.cfg, c)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("New error %q does not name %q", err.Error(), tt.want)
			}
		})
	}
}

// TestNoSchemeMeansHTTPS pins the documented default without a network call.
func TestNoSchemeMeansHTTPS(t *testing.T) {
	plain := mustClient(t, Config{Endpoint: "minio.internal:9000", Bucket: testBucket, Region: testRegion, AccessKeyID: "k", SecretAccessKey: "s"})
	if got := plain.urlFor("k", nil); got != "https://minio.internal:9000/recovery-kit/k" {
		t.Errorf("urlFor = %q, want an https endpoint", got)
	}
	insecure := mustClient(t, Config{Endpoint: "http://minio.internal:9000", Bucket: testBucket, Region: testRegion, AccessKeyID: "k", SecretAccessKey: "s"})
	if got := insecure.urlFor("k", nil); got != "http://minio.internal:9000/recovery-kit/k" {
		t.Errorf("urlFor = %q, want the given http scheme kept", got)
	}
}

// An unprefixed listing must send NO prefix parameter. S3 policies test
// s3:prefix, and `prefix=` (present but empty) is a different request from one
// without it: a policy whose ListBucket grant is conditioned on s3:prefix being
// absent refuses the first and allows the second. The scope check's unprefixed
// probe asks what any client can do with the credential, so it must send the
// request a client that lists the whole bucket sends.
func TestAnUnprefixedListSendsNoPrefixParameter(t *testing.T) {
	rec := &recorder{handler: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	c := mustClient(t, Config{
		Endpoint:        srv.URL,
		Bucket:          testBucket,
		Region:          testRegion,
		AccessKeyID:     testAccessKey,
		SecretAccessKey: testSecretKey,
	})

	if _, _, err := c.List(context.Background(), ""); err != nil {
		t.Fatalf("List with no prefix: %v", err)
	}
	if _, present := rec.last(t).query["prefix"]; present {
		t.Errorf("an unprefixed List sent a prefix parameter (query %q); the request a policy sees as unprefixed carries none", rec.last(t).rawQuery)
	}

	if _, _, err := c.List(context.Background(), "clusters/a/"); err != nil {
		t.Fatalf("List with a prefix: %v", err)
	}
	if got := rec.last(t).query.Get("prefix"); got != "clusters/a/" {
		t.Errorf("prefix parameter = %q, want %q", got, "clusters/a/")
	}
}
