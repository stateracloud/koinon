// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/stateracloud/koinon/internal/forgecredfile"
	"github.com/stateracloud/koinon/internal/gateway/credkind"
	"github.com/stateracloud/koinon/internal/gateway/route"
	"github.com/stateracloud/koinon/internal/gatewaykeys"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// CredentialOpener reads the value of a credential that a route names (#1086 part C). It runs on every
// request of a credentialed route, so a rotated file applies at once. The value never leaves the
// gateway: it goes upstream in the Authorization header and nowhere else.
type CredentialOpener interface {
	Open(c route.Credential) (string, error)
}

// OpenError is the failure to read a sealed credential. The request is refused and nothing goes
// upstream. Reason is one of the fixed reasons of [gatewaykeys.Metrics]. The text holds no byte of the
// envelope or the credential.
type OpenError struct {
	Reason string
	Err    error
}

func (e *OpenError) Error() string {
	return fmt.Sprintf("egress: the sealed credential cannot be opened (%s)", e.Reason)
}

func (e *OpenError) Unwrap() error { return e.Err }

// maxEnvelope bounds the read of a credential file. A v2 envelope of a forge token is far smaller.
const maxEnvelope = 64 << 10

// SealedCredentials opens forge credential files (package forgecredfile): v2 envelopes that only the
// gateway key opens (ADR 0097 decision 12). Any other file is refused: the egress routes never read a
// plaintext credential.
type SealedCredentials struct {
	// Keys are the gateway's recipient keys. Nil refuses every credential.
	Keys *secretstore.RecipientKeyring
	// OnOpenFailure counts a refused credential by its fixed reason. It may be nil.
	OnOpenFailure func(reason string)
}

// Open implements CredentialOpener.
func (s SealedCredentials) Open(c route.Credential) (string, error) {
	v, err := s.open(c)
	if err != nil {
		var oe *OpenError
		if s.OnOpenFailure != nil && errors.As(err, &oe) {
			s.OnOpenFailure(oe.Reason)
		}
		return "", err
	}
	return v, nil
}

func (s SealedCredentials) open(c route.Credential) (string, error) {
	base := filepath.Base(c.File)
	org, agent, ok := forgecredfile.ParseSealed(base)
	// The path is absolute and clean, so it names exactly the file of the key mount the State gives.
	if !ok || agent != c.Agent || !filepath.IsAbs(c.File) || filepath.Clean(c.File) != c.File {
		// The file name binds the envelope to one agent; a file of another agent is never sent.
		return "", &OpenError{Reason: gatewaykeys.ReasonFormat, Err: secretstore.ErrUnsupportedFormat}
	}
	f, err := os.Open(c.File)
	if err != nil {
		// The error names the file, never its content.
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxEnvelope+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxEnvelope {
		return "", &OpenError{Reason: gatewaykeys.ReasonFormat, Err: secretstore.ErrUnsupportedFormat}
	}
	var env secretstore.EnvelopeV2
	if err := json.Unmarshal(b, &env); err != nil || env.Format != secretstore.FormatV2 {
		return "", &OpenError{Reason: gatewaykeys.ReasonFormat, Err: secretstore.ErrUnsupportedFormat}
	}
	if s.Keys == nil {
		return "", &OpenError{Reason: gatewaykeys.ReasonUnknownKey, Err: secretstore.ErrUnknownRecipientKey}
	}
	plain, err := s.Keys.Open(forgecredfile.Binding(org, agent), env)
	if err != nil {
		reason := gatewaykeys.ReasonOpen
		switch {
		case errors.Is(err, secretstore.ErrUnknownRecipientKey):
			reason = gatewaykeys.ReasonUnknownKey
		case errors.Is(err, secretstore.ErrUnsupportedFormat):
			reason = gatewaykeys.ReasonFormat
		}
		return "", &OpenError{Reason: reason, Err: err}
	}
	defer clear(plain)
	v := strings.TrimRight(string(plain), "\r\n")
	if !headerSafe(v) {
		return "", &OpenError{Reason: gatewaykeys.ReasonFormat, Err: secretstore.ErrUnsupportedFormat}
	}
	return v, nil
}

// headerSafe says v can go in a header: not empty, printable ASCII, no space. A token never has more.
func headerSafe(v string) bool {
	if v == "" {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] <= ' ' || v[i] >= 0x7f {
			return false
		}
	}
	return true
}

// attached is a credential ready for one request: the header value and the strings that must never
// come back to the guest.
type attached struct {
	// kind names the credential kind for the scrub (credkind, #1221).
	kind    credkind.Kind
	header  string
	needles [][]byte
}

// minNeedle is the shortest value the gateway scrubs. A shorter credential is refused at attach: a
// short value would mask ordinary bytes of the response.
const minNeedle = 8

func attach(c route.Credential, value string) (attached, error) {
	if len(value) < minNeedle {
		return attached{}, &OpenError{Reason: gatewaykeys.ReasonFormat, Err: secretstore.ErrUnsupportedFormat}
	}
	switch c.Scheme {
	case route.SchemeBasic:
		pair := base64.StdEncoding.EncodeToString([]byte(c.User + ":" + value))
		return attached{kind: credkind.EgressBasic, header: "Basic " + pair, needles: needles(value, pair)}, nil
	case route.SchemeBearer:
		return attached{kind: credkind.EgressBearer, header: "Bearer " + value, needles: needles(value)}, nil
	}
	return attached{}, &OpenError{Reason: gatewaykeys.ReasonFormat, Err: secretstore.ErrUnsupportedFormat}
}

func needles(values ...string) [][]byte {
	out := make([][]byte, 0, len(values))
	for _, v := range values {
		out = append(out, []byte(v))
	}
	return out
}

// mask replaces every needle in b with '*' of the same length, so a Content-Length stays right.
func mask(b []byte, needles [][]byte) bool {
	hit := false
	for _, n := range needles {
		for off := 0; ; {
			i := bytes.Index(b[off:], n)
			if i < 0 {
				break
			}
			hit = true
			start := off + i
			for j := range n {
				b[start+j] = '*'
			}
			off = start + len(n)
		}
	}
	return hit
}

// scrubBody masks the credential in a response body as it streams. It holds back the last
// longest-needle-minus-one bytes of each read, so a value split across two reads is caught too.
type scrubBody struct {
	src     io.ReadCloser
	needles [][]byte
	keep    int
	chunk   []byte
	buf     []byte // bytes read from src and not yet returned
	ready   int    // the prefix of buf that is masked and safe to return
	eof     bool
	err     error
	onHit   func()
	// trailer gives the response's trailer map. Its values arrive only when src reaches its end, so they
	// are masked there, before the proxy copies them after the body. It is read at the end, not before:
	// the transport sets a new map there when the upstream declared no trailer.
	trailer func() http.Header
}

func newScrubBody(src io.ReadCloser, needles [][]byte, trailer func() http.Header, onHit func()) *scrubBody {
	keep := 0
	for _, n := range needles {
		keep = max(keep, len(n)-1)
	}
	return &scrubBody{src: src, needles: needles, keep: keep, trailer: trailer, onHit: onHit}
}

// maskHeader masks the needles in every value of h in place. It reports whether one was found.
func maskHeader(h http.Header, needles [][]byte) bool {
	hit := false
	for k, vs := range h {
		for i := range vs {
			b := []byte(vs[i])
			if mask(b, needles) {
				hit = true
				h[k][i] = string(b)
			}
		}
	}
	return hit
}

func (s *scrubBody) Read(p []byte) (int, error) {
	for s.ready == 0 {
		if s.eof {
			if len(s.buf) == 0 {
				return 0, s.err
			}
			s.ready = len(s.buf)
			break
		}
		if s.chunk == nil {
			s.chunk = make([]byte, 32<<10)
		}
		n, err := s.src.Read(s.chunk)
		s.buf = append(s.buf, s.chunk[:n]...)
		hit := false
		if err != nil {
			s.eof, s.err = true, err
			hit = maskHeader(s.trailer(), s.needles)
		}
		if (mask(s.buf, s.needles) || hit) && s.onHit != nil {
			s.onHit()
		}
		if s.eof {
			s.ready = len(s.buf)
		} else if len(s.buf) > s.keep {
			s.ready = len(s.buf) - s.keep
		}
	}
	n := copy(p, s.buf[:s.ready])
	s.buf = s.buf[n:]
	s.ready -= n
	if len(s.buf) == 0 && s.eof {
		return n, s.err
	}
	return n, nil
}

func (s *scrubBody) Close() error { return s.src.Close() }
