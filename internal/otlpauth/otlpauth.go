// SPDX-License-Identifier: Apache-2.0

// Package otlpauth authenticates the requests to koinon-api's internal OTLP listener (logs from
// agentgateway, traces from agentgateway and koinon-agentd) with a shared bearer token.
//
// The token is a Secret the chart's ensure-secrets Job generates and the pods mount as files. The
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

	mu       sync.Mutex
	loaded   time.Time
	digests  [][sha256.Size]byte
	now      func() time.Time
	lastFail string
}

// NewTokens returns the tokens in dir. It fails when dir has no valid "token" file, so a
// misconfigured listener does not start (and so is never open).
func NewTokens(dir string, log *slog.Logger) (*Tokens, error) {
	if dir == "" {
		return nil, errors.New("the OTLP token directory is required")
	}
	if log == nil {
		log = slog.Default()
	}
	t := &Tokens{Reload: DefaultReload, dir: dir, log: log, now: time.Now}
	d, err := t.read()
	if err != nil {
		return nil, err
	}
	t.digests, t.loaded = d, t.now()
	return t, nil
}

// read loads the current token (required) and the previous one (optional).
func (t *Tokens) read() ([][sha256.Size]byte, error) {
	cur, err := ReadToken(filepath.Join(t.dir, TokenFile))
	if err != nil {
		return nil, err
	}
	out := [][sha256.Size]byte{sha256.Sum256([]byte(cur))}
	prev, err := ReadToken(filepath.Join(t.dir, PreviousFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		out = append(out, sha256.Sum256([]byte(prev)))
	}
	return out, nil
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
// old. A failed re-read keeps the last good tokens (a Secret mid-update must not lock the senders
// out) and is logged once per distinct error.
func (t *Tokens) accepted() [][sha256.Size]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.now().Sub(t.loaded) >= t.Reload {
		d, err := t.read()
		t.loaded = t.now()
		switch {
		case err == nil:
			t.digests, t.lastFail = d, ""
		case err.Error() != t.lastFail:
			t.lastFail = err.Error()
			t.log.Warn("OTLP token files unreadable; keeping the tokens already loaded", "err", err)
		}
	}
	return t.digests
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
			if !tokens.Match(strings.TrimSpace(token)) {
				reject(w, reasonInvalid)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
