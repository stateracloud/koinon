// SPDX-License-Identifier: Apache-2.0

// Package otlpauth authenticates the requests to koinon-api's internal OTLP listener with bearer
// tokens, one set per scope (issue 55): the gateway's token admits /v1/logs (and is the default for
// every other path), a token only koinon-agentd holds admits /v1/traces. A token of one scope never
// admits the other, so a holder of the gateway's token cannot post spans into a traced run.
//
// Each token set is a Secret the chart's ensure-secrets Job generates and the pods mount as files. The
// Secret has two keys: "token", the one senders use, and an optional "previous", the one it replaced.
// The listener accepts either, so an operator rotates without dropping a record: put the old value
// in "previous", the new one in "token", wait until every sender uses the new one (kubelet updates
// the mounted files in place; the gateway reads its file when it loads its config), then delete
// "previous". The files are re-read every few seconds, so the listener needs no restart.
package otlpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	// TokenFile is the file in the mounted Secret that holds the current token.
	TokenFile = "token"
	// PreviousFile is the optional file that holds the token being rotated out.
	PreviousFile = "previous"

	// MinTokenLength is the shortest token accepted. Generated tokens are 64 hex characters.
	MinTokenLength = 32

	// DefaultReload bounds how often the files are read.
	DefaultReload = 5 * time.Second
)

// Rejection reasons of koinon_api_otlp_auth_rejected_total: a fixed set.
const (
	reasonMissing = "missing" // no Authorization header, or not a Bearer one
	reasonInvalid = "invalid" // a token that is neither the current nor the previous one
)

// tokenRE is the characters of a header-safe token (RFC 6750 token68 without padding games).
var tokenRE = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`)

// Tokens reads the accepted tokens from the files of a mounted Secret.
type Tokens struct {
	// Reload is how often the files are re-read (default DefaultReload).
	Reload time.Duration

	dir string
	log *slog.Logger

	mu      sync.Mutex
	loaded  time.Time
	cur     [sha256.Size]byte // the last valid "token"
	prev    [sha256.Size]byte // the valid "previous", when hasPrev
	hasPrev bool
	now     func() time.Time
	// lastFail is the last logged problem per file, so a persistent one is logged once.
	lastFail map[string]string
}

// NewTokens returns the tokens in dir. It fails when dir has no valid "token" file, so a
// misconfigured listener does not start (and so is never open). A bad "previous" never blocks the
// start: it is logged and not accepted.
func NewTokens(dir string, log *slog.Logger) (*Tokens, error) {
	if dir == "" {
		return nil, errors.New("the OTLP token directory is required")
	}
	if log == nil {
		log = slog.Default()
	}
	t := &Tokens{Reload: DefaultReload, dir: dir, log: log, now: time.Now, lastFail: map[string]string{}}
	cur, err := ReadToken(filepath.Join(t.dir, TokenFile))
	if err != nil {
		return nil, err
	}
	t.cur = sha256.Sum256([]byte(cur))
	t.readPrevious()
	t.loaded = t.now()
	return t, nil
}

// readPrevious loads the optional "previous" token: absent, empty and invalid all mean none (an
// invalid one is logged without its value). It is validated apart from "token", so a bad
// "previous" never blocks a freshly rotated "token".
func (t *Tokens) readPrevious() {
	t.hasPrev = false
	path := filepath.Join(t.dir, PreviousFile)
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == "" { //nolint:gosec // an operator-set directory
		t.fail(PreviousFile, nil)
		return
	}
	v, err := ReadToken(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.fail(PreviousFile, err)
		}
		return
	}
	t.fail(PreviousFile, nil)
	t.prev, t.hasPrev = sha256.Sum256([]byte(v)), true
}

// fail logs err once per distinct error for file; nil clears it.
func (t *Tokens) fail(file string, err error) {
	switch {
	case err == nil:
		delete(t.lastFail, file)
	case err.Error() != t.lastFail[file]:
		t.lastFail[file] = err.Error()
		t.log.Warn("OTLP token file not usable", "file", file, "err", err)
	}
}

// ReadToken returns the token in the file at path, for a sender (koinon-agentd) to put in
// "Authorization: Bearer". It fails, naming the rule and never the value, when the file is missing
// or does not hold a valid token. An exporter should call it for each export, so a rotated token
// is picked up without a restart.
func ReadToken(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // an operator-set directory
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return "", fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	s := strings.TrimSpace(string(b))
	if len(s) < MinTokenLength || !tokenRE.MatchString(s) {
		// Never the value: only the file and the rule.
		return "", fmt.Errorf("%s must hold a token of at least %d characters from A-Z a-z 0-9 . _ ~ + / = -", filepath.Base(path), MinTokenLength)
	}
	return s, nil
}

// accepted returns the digests of the accepted tokens, re-reading the files when the last read is
// old. An unreadable or invalid "token" keeps the last good one (a Secret mid-update must not lock
// the senders out); "previous" is accepted only while it is valid.
func (t *Tokens) accepted() [][sha256.Size]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.now().Sub(t.loaded) >= t.Reload {
		t.loaded = t.now()
		if cur, err := ReadToken(filepath.Join(t.dir, TokenFile)); err != nil {
			t.fail(TokenFile, err)
		} else {
			t.fail(TokenFile, nil)
			t.cur = sha256.Sum256([]byte(cur))
		}
		t.readPrevious()
	}
	out := [][sha256.Size]byte{t.cur}
	if t.hasPrev {
		out = append(out, t.prev)
	}
	return out
}

// Match reports whether bearer is an accepted token. It compares SHA-256 digests in constant time
// and always tests every accepted token.
func (t *Tokens) Match(bearer string) bool {
	got := sha256.Sum256([]byte(bearer))
	ok := 0
	for _, want := range t.accepted() {
		ok |= subtle.ConstantTimeCompare(got[:], want[:])
	}
	return ok == 1
}

// SharesToken reports whether t and other accept a common token (current or previous). Two scopes
// that share one token defeat the split (issue 55), so koinon-api refuses to start on it. The
// digests are compared in constant time and every pair is tested.
func (t *Tokens) SharesToken(other *Tokens) bool {
	shared := 0
	for _, a := range t.accepted() {
		for _, b := range other.accepted() {
			shared |= subtle.ConstantTimeCompare(a[:], b[:])
		}
	}
	return shared == 1
}

// Metrics are the receiver's authentication metrics.
type Metrics struct {
	// Rejected counts refused requests by reason, a fixed set; never by sender or path.
	Rejected *prometheus.CounterVec
}

// NewMetrics creates the metrics and registers them with reg (nil: not registered).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{Rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "koinon_api_otlp_auth_rejected_total",
		Help: "Requests to the internal OTLP listener refused for a missing or wrong bearer token, by reason.",
	}, []string{"reason"})}
	for _, r := range []string{reasonMissing, reasonInvalid} {
		m.Rejected.WithLabelValues(r)
	}
	if reg != nil {
		reg.MustRegister(m.Rejected)
	}
	return m
}

// Middleware wraps next so that only requests with an accepted bearer token reach it. Every path is
// covered, including ones next does not serve. A refusal is a 401 with no body and no detail, and
// the token is never logged.
func Middleware(tokens *Tokens, m *Metrics) func(http.Handler) http.Handler {
	return ScopedMiddleware(tokens, nil, m)
}

// ScopedMiddleware is Middleware with a token set per path (issue 55). A request whose URL path is
// a key of scoped is checked against that path's tokens only; every other path, including ones next
// does not serve, is checked against fallback. A token of one scope therefore gets a 401 on the
// other: the gateway's token on /v1/traces, and agentd's token on /v1/logs or anywhere else.
// The path is matched exactly, as the handlers behind it match it, so a variant such as
// "/v1/traces/" is no bypass: it falls to fallback and is refused unless fallback's token is sent.
func ScopedMiddleware(fallback *Tokens, scoped map[string]*Tokens, m *Metrics) func(http.Handler) http.Handler {
	reject := func(w http.ResponseWriter, reason string) {
		if m != nil {
			m.Rejected.WithLabelValues(reason).Inc()
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.WriteHeader(http.StatusUnauthorized)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
			if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
				reject(w, reasonMissing)
				return
			}
			tokens := fallback
			if t, ok := scoped[r.URL.Path]; ok {
				tokens = t
			}
			if !tokens.Match(strings.TrimSpace(token)) {
				reject(w, reasonInvalid)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
