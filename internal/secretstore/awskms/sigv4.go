// SPDX-License-Identifier: Apache-2.0

package awskms

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

// Signature Version 4 (https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_sigv-create-signed-request.html),
// the part the adapter needs: a request with its whole body in memory, signed in the Authorization
// header. sigv4_test.go checks it against AWS's published test vectors.

const (
	sigAlgorithm = "AWS4-HMAC-SHA256"
	amzDate      = "20060102T150405Z"
)

// sign signs req, whose body is body, for service in region with c at now. It sets X-Amz-Date,
// X-Amz-Security-Token (temporary credentials) and Authorization. Every header already on req is
// signed, and the host; headers the transport adds later (User-Agent, Content-Length) are not.
func sign(req *http.Request, body []byte, c credentials, region, service string, now time.Time) {
	now = now.UTC()
	req.Header.Set("X-Amz-Date", now.Format(amzDate))
	if c.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	headers, signed := canonicalHeaders(req)
	payload := sha256.Sum256(body)
	canonical := strings.Join([]string{
		req.Method,
		canonicalPath(req.URL),
		canonicalQuery(req.URL),
		headers,
		signed,
		hex.EncodeToString(payload[:]),
	}, "\n")
	day := now.Format("20060102")
	scope := day + "/" + region + "/" + service + "/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := sigAlgorithm + "\n" + now.Format(amzDate) + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	key := signingKey(c.SecretAccessKey, day, region, service)
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	clear(key)
	req.Header.Set("Authorization", sigAlgorithm+" Credential="+c.AccessKeyID+"/"+scope+
		", SignedHeaders="+signed+", Signature="+sig)
}

// signingKey derives the day's key for region and service from the secret access key.
func signingKey(secret, day, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(data))
	return m.Sum(nil)
}

// canonicalHeaders returns the canonical header block (each "name:value\n", names lower case and
// sorted, values trimmed with inner runs of spaces collapsed, repeated headers joined by commas) and
// the signed header list.
func canonicalHeaders(req *http.Request) (string, string) {
	vals := map[string][]string{}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	vals["host"] = []string{host}
	for name, vs := range req.Header {
		n := strings.ToLower(name)
		if n == "authorization" {
			continue
		}
		vals[n] = append(vals[n], vs...)
	}
	names := make([]string, 0, len(vals))
	for n := range vals {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		trimmed := make([]string, len(vals[n]))
		for i, v := range vals[n] {
			trimmed[i] = strings.Join(strings.Fields(v), " ")
		}
		b.WriteString(n + ":" + strings.Join(trimmed, ",") + "\n")
	}
	return b.String(), strings.Join(names, ";")
}

// canonicalPath is the URI-encoded path ("/" when empty). The adapter only ever posts to "/".
func canonicalPath(u *url.URL) string {
	p := u.Path
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	return strings.Join(segs, "/")
}

// canonicalQuery is the query with keys and values URI-encoded, sorted by key, then value.
func canonicalQuery(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	q, _ := url.ParseQuery(u.RawQuery)
	type pair struct{ k, v string }
	pairs := make([]pair, 0, len(q))
	for k, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, pair{uriEncode(k), uriEncode(v)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.k + "=" + p.v
	}
	return strings.Join(out, "&")
}

// uriEncode percent-encodes every byte except the unreserved characters (RFC 3986), in upper-case
// hex, as SigV4 wants.
func uriEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&15])
	}
	return b.String()
}
