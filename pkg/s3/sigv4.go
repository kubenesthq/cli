package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// sigV4Algorithm is the signing algorithm named in the Authorization header.
const sigV4Algorithm = "AWS4-HMAC-SHA256"

// sigV4Service is the service name in the credential scope: this package
// signs for S3 only.
const sigV4Service = "s3"

// iso8601 is the X-Amz-Date format (basic ISO 8601, UTC).
const iso8601 = "20060102T150405Z"

// shortDate is the date stamp in the credential scope.
const shortDate = "20060102"

// sign signs req in place for service "s3" and returns the Authorization
// header value. payloadHash is the lowercase hex SHA-256 of the body, which is
// also sent as x-amz-content-sha256 — S3 requires that header on every
// request. now is the signing instant; the request's X-Amz-Date is rewritten
// from it so the header and the signature can never disagree.
func (c *Client) sign(req *http.Request, payloadHash string, now time.Time) string {
	now = now.UTC()
	amzDate := now.Format(iso8601)
	dateStamp := now.Format(shortDate)

	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("X-Amz-Date", amzDate)

	// The canonical headers block already ends in a newline; the leading
	// newline of the next list item is the blank line the format calls for.
	canonicalHeaders, signedHeaders := canonicalHeaders(req)
	scope := strings.Join([]string{dateStamp, c.region, sigV4Service, "aws4_request"}, "/")

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.Path),
		canonicalQueryString(req.URL.RawQuery),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		amzDate,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(signingKey(c.secretKey, dateStamp, c.region), stringToSign))

	auth := sigV4Algorithm + " Credential=" + c.accessKeyID + "/" + scope +
		",SignedHeaders=" + signedHeaders + ",Signature=" + signature
	req.Header.Set("Authorization", auth)
	return auth
}

// canonicalHeaders returns the canonical-headers block (each header, low-
// ercased name and colon, canonicalized value, trailing newline) and the
// semicolon-joined sorted names. Every header the request carries is signed,
// plus Host — which net/http keeps out of Header and writes from Request.Host.
// Values are trimmed, runs of whitespace collapsed, and repeated values joined
// with a single space.
func canonicalHeaders(req *http.Request) (string, string) {
	values := make(map[string][]string, len(req.Header)+1)
	for name, vals := range req.Header {
		values[strings.ToLower(name)] = vals
	}
	host := req.Host
	if host == "" && req.URL != nil {
		host = req.URL.Host
	}
	values["host"] = []string{host}

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		canonical := make([]string, 0, len(values[name]))
		for _, v := range values[name] {
			canonical = append(canonical, collapseSpaces(v))
		}
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(strings.Join(canonical, " "))
		b.WriteByte('\n')
	}
	return b.String(), strings.Join(names, ";")
}

// collapseSpaces trims v and collapses each run of whitespace to one space.
func collapseSpaces(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

// canonicalURI URI-encodes a request path per RFC 3986, keeping "/" as the
// segment separator.
func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	return uriEncode(path, false)
}

// canonicalQueryString re-encodes a request query string in canonical form:
// names and values RFC 3986-encoded, sorted by name then value, joined "a=b"
// with "&", a parameter with no value rendered as "a=". The query strings this
// package builds are already in that form, so the round trip through
// url.ParseQuery is exact.
func canonicalQueryString(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	// A malformed parameter is dropped; url.ParseQuery returns the rest, and a
	// request is better signed than not signed at all.
	values, _ := url.ParseQuery(rawQuery)
	return canonicalQuery(values)
}

// canonicalQuery renders query parameters in canonical form.
func canonicalQuery(values url.Values) string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, name := range names {
		vals := append([]string(nil), values[name]...)
		sort.Strings(vals)
		if len(vals) == 0 {
			vals = []string{""}
		}
		for _, v := range vals {
			parts = append(parts, uriEncode(name, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// unreserved reports whether c is an RFC 3986 unreserved character, which
// never needs escaping.
func unreserved(c byte) bool {
	return c >= 'A' && c <= 'Z' ||
		c >= 'a' && c <= 'z' ||
		c >= '0' && c <= '9' ||
		c == '-' || c == '_' || c == '.' || c == '~'
}

// uriEncode percent-encodes s the way SigV4 canonicalization requires: only
// unreserved characters survive, everything else becomes an uppercase %XX
// escape (a space is %20, never "+"). When encodeSlash is false the "/"
// separators of a path are preserved.
func uriEncode(s string, encodeSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case unreserved(c):
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte('/')
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}

// hexSHA256 is the lowercase hex SHA-256 of b. The empty (nil) body hashes to
// e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855.
func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// hmacSHA256 is HMAC-SHA256(key, data).
func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// signingKey derives the date/region/service-scoped key SigV4 signs with.
func signingKey(secret, dateStamp, region string) []byte {
	key := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, sigV4Service)
	return hmacSHA256(key, "aws4_request")
}
