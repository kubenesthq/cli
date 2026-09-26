// Package s3 is the CLI's S3 client: the handful of REST operations the
// recovery-kit flow needs — put, conditional put, get, head, one page of list,
// whether the bucket exists, and the two bucket-protection reads —
// authenticated with AWS Signature Version 4.
//
// It exists because the CLI ships no AWS SDK. Signing is implemented over
// net/http in sigv4.go; the only dependency is the standard library.
package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The sentinel errors callers discriminate on. An *Error unwraps to at most
// one of them, so errors.Is(err, ErrNotFound) is the whole test.
var (
	ErrNotFound           = errors.New("s3: no such object")
	ErrPreconditionFailed = errors.New("s3: the object already exists")
	ErrAccessDenied       = errors.New("s3: access denied")
)

// Config locates one bucket.
type Config struct {
	// Endpoint is the S3 API endpoint, with or without a scheme; no scheme
	// means https.
	Endpoint        string
	Bucket          string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	// HTTPClient is optional; a default with a 30s timeout is used.
	HTTPClient *http.Client
	// Now is optional; time.Now is used. Inject it in tests.
	Now func() time.Time
}

// defaultTimeout bounds one HTTP exchange (connect, send, receive) when
// Config.HTTPClient is nil. Recovery kits are small; a hung endpoint must not
// wedge the CLI.
const defaultTimeout = 30 * time.Second

// Object is one object's metadata, as Head returns it.
type Object struct {
	Size int64
	ETag string
}

// Protection is a bucket's default server-side encryption, as
// BucketEncryption returns it.
type Protection struct {
	// Algorithm is the default SSE algorithm ("AES256", "aws:kms", …), or ""
	// when the bucket has no default.
	Algorithm string
}

// Versioning is a bucket's versioning state, as BucketVersioning returns it.
type Versioning struct {
	// Status is "Enabled", "Suspended", or "" when versioning was never
	// configured.
	Status string
}

// Error is one refused S3 operation.
type Error struct {
	Op         string // "PutObject", "GetObject", "ListObjectsV2", "HeadObject", "HeadBucket", "GetBucketEncryption", "GetBucketVersioning"
	Key        string // object key, or "" for a bucket-level call
	StatusCode int
	Code       string // the S3 error Code element, e.g. "AccessDenied", "NoSuchKey", "PreconditionFailed"
	Message    string
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("s3 ")
	b.WriteString(e.Op)
	if e.Key != "" {
		b.WriteByte(' ')
		b.WriteString(strconv.Quote(e.Key))
	}
	b.WriteString(": ")
	code := e.Code
	if code == "" {
		if code = http.StatusText(e.StatusCode); code == "" {
			code = "HTTPError"
		}
	}
	fmt.Fprintf(&b, "%s (%d)", code, e.StatusCode)
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

// Unwrap maps the response onto the sentinel errors. The status code decides
// first: a HEAD refusal carries no body at all, so no Code is available to
// classify it by.
func (e *Error) Unwrap() error {
	switch {
	case e.StatusCode == http.StatusNotFound, e.Code == "NoSuchKey", e.Code == "NotFound":
		return ErrNotFound
	case e.StatusCode == http.StatusPreconditionFailed, e.Code == "PreconditionFailed":
		return ErrPreconditionFailed
	case e.StatusCode == http.StatusForbidden, e.Code == "AccessDenied":
		return ErrAccessDenied
	}
	return nil
}

// Client is one bucket on one S3-compatible endpoint.
type Client struct {
	scheme        string
	host          string // host[:port], lowercase; the endpoint without the bucket
	basePath      string // the endpoint's own path prefix, without a trailing "/"
	bucket        string
	region        string
	accessKeyID   string
	secretKey     string
	virtualHosted bool // AWS endpoints only; see onAWS
	http          *http.Client
	now           func() time.Time
}

// New validates cfg and returns a client for its bucket. It performs no
// request: whether the credentials actually open the bucket is the caller's
// first operation, not an assumption made here.
func New(cfg Config) (*Client, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	bucket := strings.TrimSpace(cfg.Bucket)
	region := strings.TrimSpace(cfg.Region)
	switch {
	case endpoint == "":
		return nil, errors.New("s3: an endpoint is required")
	case bucket == "":
		return nil, errors.New("s3: a bucket is required")
	case region == "":
		return nil, errors.New("s3: a region is required")
	case cfg.AccessKeyID == "":
		return nil, errors.New("s3: an access key ID is required")
	case cfg.SecretAccessKey == "":
		return nil, errors.New("s3: a secret access key is required")
	}

	raw := endpoint
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw // no scheme means https
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("s3: endpoint %q is not a usable URL: %w", cfg.Endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("s3: endpoint %q must be http or https, not %q", cfg.Endpoint, u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("s3: endpoint %q has no host", cfg.Endpoint)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("s3: endpoint %q must not carry a query or a fragment", cfg.Endpoint)
	}

	c := &Client{
		scheme:        u.Scheme,
		host:          strings.ToLower(u.Host), // the Host header and the signature must agree
		basePath:      strings.TrimSuffix(u.EscapedPath(), "/"),
		bucket:        bucket,
		region:        region,
		accessKeyID:   cfg.AccessKeyID,
		secretKey:     cfg.SecretAccessKey,
		virtualHosted: onAWS(u.Hostname()),
		http:          cfg.HTTPClient,
		now:           cfg.Now,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: defaultTimeout}
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// onAWS reports whether the endpoint is AWS itself. This duplicates
// backup.Target.onAWS (kubenest-cli/pkg/backup/target.go) deliberately: pkg/s3
// must not import pkg/backup, and the rule is one hostname match. Everything
// that is not AWS — MinIO, Ceph, B2, … — gets path-style addressing, because
// virtual-host style needs wildcard DNS most stores don't have.
func onAWS(host string) bool {
	host = strings.ToLower(host)
	return host == "amazonaws.com" || strings.HasSuffix(host, ".amazonaws.com")
}

// urlFor builds the absolute URL of key ("" for a bucket-level call) with the
// given query parameters, path and query already RFC 3986-encoded.
func (c *Client) urlFor(key string, query url.Values) string {
	host := c.host
	path := c.basePath
	if c.virtualHosted {
		host = c.bucket + "." + host
	} else {
		path += "/" + uriEncode(c.bucket, false)
	}
	if key = strings.TrimPrefix(key, "/"); key != "" {
		path += "/" + uriEncode(key, false)
	}
	if path == "" {
		path = "/"
	}

	raw := c.scheme + "://" + host + path
	if q := canonicalQuery(query); q != "" {
		raw += "?" + q
	}
	return raw
}

// newRequest builds and signs one request. header is applied before signing,
// so a conditional header is covered by the signature.
func (c *Client) newRequest(ctx context.Context, method, key string, body []byte, query url.Values, header map[string]string) (*http.Request, error) {
	raw := c.urlFor(key, query)
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("s3: building %s request: %w", method, err)
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, raw, reader)
	if err != nil {
		return nil, fmt.Errorf("s3: building %s request: %w", method, err)
	}
	// Use exactly the URL urlFor built, encoding and all: re-parsing it must
	// not be allowed to normalize the request line away from the signature.
	req.URL = u
	for name, value := range header {
		req.Header.Set(name, value)
	}
	c.sign(req, hexSHA256(body), c.now())
	return req, nil
}

// response is one completed HTTP exchange, body already read and closed.
type response struct {
	status int
	header http.Header
	body   []byte
}

// ok reports whether the status is 2xx.
func (r *response) ok() bool { return r.status >= 200 && r.status < 300 }

// configAbsent reports whether a non-2xx bucket-configuration response means
// "this bucket has no such configuration" rather than a refusal. A bucket with
// no default encryption is a fact, not a failure.
func (r *response) configAbsent() bool {
	if r.status == http.StatusNotFound {
		return true
	}
	var doc s3ErrorDoc
	if err := unmarshalXML(r.body, &doc); err != nil {
		return false
	}
	switch doc.Code {
	case "ServerSideEncryptionConfigurationNotFoundError", "NoSuchBucketConfiguration", "NoSuchVersioningConfiguration":
		return true
	}
	return false
}

// do performs req and reads its whole body.
func (c *Client) do(req *http.Request) (*response, error) {
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3 %s %s: %w", req.Method, req.URL.Redacted(), err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("s3 %s %s: reading response: %w", req.Method, req.URL.Redacted(), err)
	}
	return &response{status: res.StatusCode, header: res.Header, body: body}, nil
}

// s3ErrorDoc is the S3 REST error document.
type s3ErrorDoc struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

// newError classifies one non-2xx response. Server errors come back as an
// error document; anything else (a proxy's HTML page, say) keeps its status
// code, which is what Unwrap falls back on.
func (c *Client) newError(op, key string, status int, body []byte) *Error {
	e := &Error{Op: op, Key: key, StatusCode: status}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return e
	}
	var doc s3ErrorDoc
	if err := xml.Unmarshal(trimmed, &doc); err == nil {
		e.Code, e.Message = doc.Code, doc.Message
		return e
	}
	if line, _, _ := strings.Cut(string(trimmed), "\n"); line != "" {
		const limit = 200
		if len(line) > limit {
			line = line[:limit]
		}
		e.Message = line
	}
	return e
}

// unmarshalXML parses body into v, treating an empty body as an empty
// document: several S3-compatibles answer 200 with no body where AWS sends an
// empty element.
func unmarshalXML(body []byte, v any) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil
	}
	if err := xml.Unmarshal(trimmed, v); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}
	return nil
}

// Put stores body at key, overwriting whatever is there.
func (c *Client) Put(ctx context.Context, key string, body []byte) error {
	req, err := c.newRequest(ctx, http.MethodPut, key, body, nil, nil)
	if err != nil {
		return err
	}
	res, err := c.do(req)
	if err != nil {
		return err
	}
	if !res.ok() {
		return c.newError("PutObject", key, res.status, res.body)
	}
	return nil
}

// PutIfAbsent creates key only if it does not exist, by sending
// If-None-Match: * — S3's real conditional create. A 412 means somebody else
// created the object first: that is an outcome, not a failure, so it is
// reported as (false, nil).
func (c *Client) PutIfAbsent(ctx context.Context, key string, body []byte) (created bool, err error) {
	req, err := c.newRequest(ctx, http.MethodPut, key, body, nil, map[string]string{"If-None-Match": "*"})
	if err != nil {
		return false, err
	}
	res, err := c.do(req)
	if err != nil {
		return false, err
	}
	if res.ok() {
		return true, nil
	}
	if res.status == http.StatusPreconditionFailed {
		return false, nil
	}
	return false, c.newError("PutObject", key, res.status, res.body)
}

// Get returns the object's bytes.
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	req, err := c.newRequest(ctx, http.MethodGet, key, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return nil, c.newError("GetObject", key, res.status, res.body)
	}
	return res.body, nil
}

// HeadBucket reports whether the bucket exists and the credential may see it.
//
// k3s's etcd-s3 client calls this before every datastore snapshot, so a
// credential that cannot pass it makes every snapshot fail. S3 authorises the
// bucket-level HEAD as s3:ListBucket on the bucket with no s3:prefix, which a
// prefix-conditioned ListBucket grant does not cover.
func (c *Client) HeadBucket(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodHead, "", nil, nil, nil)
	if err != nil {
		return err
	}
	res, err := c.do(req)
	if err != nil {
		return err
	}
	if !res.ok() {
		return c.newError("HeadBucket", "", res.status, res.body)
	}
	return nil
}

// Head returns the object's metadata without its bytes.
func (c *Client) Head(ctx context.Context, key string) (Object, error) {
	req, err := c.newRequest(ctx, http.MethodHead, key, nil, nil, nil)
	if err != nil {
		return Object{}, err
	}
	res, err := c.do(req)
	if err != nil {
		return Object{}, err
	}
	if !res.ok() {
		return Object{}, c.newError("HeadObject", key, res.status, res.body)
	}
	var obj Object
	obj.ETag = res.header.Get("ETag")
	if size, err := strconv.ParseInt(res.header.Get("Content-Length"), 10, 64); err == nil {
		obj.Size = size
	}
	return obj, nil
}

// List returns one page of the keys under prefix, and whether S3 truncated
// that page. It never follows NextContinuationToken: the caller decides
// whether another page is worth asking for. An empty prefix lists the whole
// bucket, and sends no prefix parameter at all: a policy tests s3:prefix, and
// `prefix=` is a different request from the unprefixed one a client makes.
func (c *Client) List(ctx context.Context, prefix string) (keys []string, truncated bool, err error) {
	query := url.Values{"list-type": {"2"}}
	if prefix != "" {
		query.Set("prefix", prefix)
	}
	req, err := c.newRequest(ctx, http.MethodGet, "", nil, query, nil)
	if err != nil {
		return nil, false, err
	}
	res, err := c.do(req)
	if err != nil {
		return nil, false, err
	}
	if !res.ok() {
		return nil, false, c.newError("ListObjectsV2", "", res.status, res.body)
	}
	var doc listBucketResult
	if err := unmarshalXML(res.body, &doc); err != nil {
		return nil, false, fmt.Errorf("s3 ListObjectsV2: %w", err)
	}
	keys = make([]string, 0, len(doc.Contents))
	for _, obj := range doc.Contents {
		keys = append(keys, obj.Key)
	}
	return keys, strings.EqualFold(strings.TrimSpace(doc.IsTruncated), "true"), nil
}

// BucketEncryption reads the bucket's default server-side encryption. A bucket
// with no default encryption is a fact, not a failure: the absent-
// configuration response becomes Protection{} and a nil error.
func (c *Client) BucketEncryption(ctx context.Context) (Protection, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "", nil, url.Values{"encryption": {""}}, nil)
	if err != nil {
		return Protection{}, err
	}
	res, err := c.do(req)
	if err != nil {
		return Protection{}, err
	}
	if !res.ok() {
		if res.configAbsent() {
			return Protection{}, nil
		}
		return Protection{}, c.newError("GetBucketEncryption", "", res.status, res.body)
	}
	var doc encryptionConfiguration
	if err := unmarshalXML(res.body, &doc); err != nil {
		return Protection{}, fmt.Errorf("s3 GetBucketEncryption: %w", err)
	}
	if len(doc.Rules) == 0 {
		return Protection{}, nil
	}
	return Protection{Algorithm: doc.Rules[0].Algorithm}, nil
}

// BucketVersioning reads the bucket's versioning state. A bucket that was
// never configured has no Status element — and some S3-compatibles answer 404
// instead — so both become Versioning{Status: ""} and a nil error. Anything
// else non-2xx, a 403 above all, is an error: the caller decides to warn
// rather than refuse.
func (c *Client) BucketVersioning(ctx context.Context) (Versioning, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "", nil, url.Values{"versioning": {""}}, nil)
	if err != nil {
		return Versioning{}, err
	}
	res, err := c.do(req)
	if err != nil {
		return Versioning{}, err
	}
	if !res.ok() {
		if res.configAbsent() {
			return Versioning{}, nil
		}
		return Versioning{}, c.newError("GetBucketVersioning", "", res.status, res.body)
	}
	var doc versioningConfiguration
	if err := unmarshalXML(res.body, &doc); err != nil {
		return Versioning{}, fmt.Errorf("s3 GetBucketVersioning: %w", err)
	}
	return Versioning{Status: strings.TrimSpace(doc.Status)}, nil
}

// listBucketResult is the ListObjectsV2 document, reduced to what one page of
// the recovery flow needs.
type listBucketResult struct {
	XMLName     xml.Name `xml:"ListBucketResult"`
	IsTruncated string   `xml:"IsTruncated"`
	Contents    []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
}

// encryptionConfiguration is the GetBucketEncryption document. Only the first
// rule's algorithm is of interest: it is the bucket default.
type encryptionConfiguration struct {
	XMLName xml.Name `xml:"ServerSideEncryptionConfiguration"`
	Rules   []struct {
		Algorithm string `xml:"ApplyServerSideEncryptionByDefault>SSEAlgorithm"`
	} `xml:"Rule"`
}

// versioningConfiguration is the GetBucketVersioning document.
type versioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Status  string   `xml:"Status"`
}
