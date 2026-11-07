// SPDX-License-Identifier: Apache-2.0

// Package scrub removes credentials and personal data from the content of run traces before the
// trace router exports it (ADR 0074, decision 6, old issue 213).
//
// The router calls it on every span attribute value. It never calls a model or the network, and
// it depends on nothing else in Koinon.
//
// Two tiers of rules:
//
//   - Always, at every level including full: Koinon gateway keys (kgw_...) and the OpenShell
//     placeholder that stands for one in a sandbox, Authorization and cookie header values,
//     bearer tokens, JWT-shaped strings, vendor API key shapes (the rule set of gitleaks, pinned
//     by version; see gitleaks.go), and the verbatim values
//     of the org's known secrets (its secret store and provider records, passed in as a Secrets set).
//   - At redacted, also: email addresses, phone numbers, IBANs (mod-97 checked), payment-card
//     numbers (Luhn checked, known issuer ranges) and high-entropy tokens.
//
// A match is replaced with [redacted:<kind>]. The scrubber works on the original text in one pass:
// every detector reports spans, overlapping spans are merged, and the text is rebuilt once, so one
// rule never sees another rule's replacement.
//
// What it does not do: it is a filter for what looks like a credential or personal data, not a
// guarantee. A secret the org never stored, in a shape no rule knows, passes at redacted. That is
// why full is an owner's decision with a badge and an audit record (ADR 0074).
package scrub

import (
	"sort"
	"strings"
)

// Level is an org's content level (ADR 0074, decision 6).
type Level int

const (
	// LevelNone is the default level. The router sends no content at it; the scrubber still applies
	// the always rules to the names and IDs that remain, so it behaves like LevelFull.
	LevelNone Level = iota
	// LevelRedacted adds the personal-data and high-entropy rules.
	LevelRedacted
	// LevelFull applies only the always rules.
	LevelFull
)

// ParseLevel maps "none", "redacted" and "full" to a Level.
func ParseLevel(s string) (Level, bool) {
	switch s {
	case "none":
		return LevelNone, true
	case "redacted":
		return LevelRedacted, true
	case "full":
		return LevelFull, true
	}
	return LevelNone, false
}

// String returns the level's name.
func (l Level) String() string {
	switch l {
	case LevelRedacted:
		return "redacted"
	case LevelFull:
		return "full"
	}
	return "none"
}

// Kind names what a replacement stood for. The kinds are a fixed set, so they are safe as a metric
// label (ADR 0023).
type Kind string

// The kinds. A replacement reads [redacted:<kind>].
const (
	KindSecret      Kind = "secret"      // verbatim value of a known org secret
	KindGatewayKey  Kind = "gateway-key" // kgw_...
	KindPlaceholder Kind = "placeholder" // openshell:resolve:env:NAME
	KindAuthHeader  Kind = "auth-header" // Authorization and similar header values
	KindCookie      Kind = "cookie"      // Cookie and Set-Cookie header values
	KindBearer      Kind = "bearer"      // Bearer <token>
	KindJWT         Kind = "jwt"
	KindAPIKey      Kind = "api-key" // well-known vendor key shapes
	KindEmail       Kind = "email"
	KindPhone       Kind = "phone"
	KindIBAN        Kind = "iban"
	KindCard        Kind = "card"
	KindToken       Kind = "token"       // high-entropy token
	KindPrivateKey  Kind = "private-key" // a PEM private key block
	KindCredential  Kind = "credential"  // the value of a key named password, token, secret ...
)

// Kinds lists every kind, for tests and metric pre-registration.
var Kinds = []Kind{
	KindSecret, KindGatewayKey, KindPlaceholder, KindAuthHeader, KindCookie, KindBearer,
	KindJWT, KindAPIKey, KindEmail, KindPhone, KindIBAN, KindCard, KindToken, KindPrivateKey, KindCredential,
}

// Replacement returns the text that replaces a match of kind k.
func Replacement(k Kind) string { return "[redacted:" + string(k) + "]" }

// Config configures a Scrubber.
type Config struct {
	// Level selects the rule set.
	Level Level
	// Secrets are the org's known secret values. Nil means none.
	Secrets *Secrets
	// OnRedaction, when set, is called once per replacement with its kind. It must be cheap and safe
	// for concurrent use; the router uses it to count redactions on a metric labelled by kind only.
	OnRedaction func(Kind)
}

// Scrubber scrubs text and JSON. It is immutable and safe for concurrent use. Building one is
// cheap, so the router makes a new one when an org's secret set or level changes.
type Scrubber struct {
	level   Level
	secrets *Secrets
	observe func(Kind)
	// leafCap, when positive, is the longest string scrubbed whole (see capLeaf); Content sets it
	// on a copy.
	leafCap int
	// work, when set, is told the length of every text a detector set reads. Only tests set it
	// (export_test.go), to count work instead of timing it.
	work func(int)
}

// New returns a Scrubber.
func New(c Config) *Scrubber {
	return &Scrubber{level: c.Level, secrets: c.Secrets, observe: c.OnRedaction}
}

// Level returns the scrubber's level.
func (s *Scrubber) Level() Level { return s.level }

// span is one match: the half-open byte range [start, end).
type span struct {
	start, end int
	kind       Kind
}

// Text scrubs plain text.
func (s *Scrubber) Text(in string) string {
	if in == "" {
		return in
	}
	var spans []span
	spans = s.collect(in, spans)
	if len(spans) == 0 {
		return in
	}
	return s.apply(in, spans)
}

// collect appends every match in in to spans.
func (s *Scrubber) collect(in string, spans []span) []span {
	spans = s.detect(in, spans)
	return detectViews(in, s.detect, spans)
}

// detect appends the spans of every detector that applies at the level, on one reading of the text.
func (s *Scrubber) detect(in string, spans []span) []span {
	if s.work != nil {
		s.work(len(in))
	}
	spans = s.secrets.find(in, spans)
	spans = findAlways(in, spans)
	if s.level == LevelRedacted {
		spans = findPersonal(in, spans)
	}
	return spans
}

// apply merges overlapping spans and rebuilds in. Overlapping spans, and touching spans of one kind, become one span
// that keeps the kind of the earliest-starting (then longest) member.
func (s *Scrubber) apply(in string, spans []span) string {
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end > spans[j].end
	})
	var b strings.Builder
	b.Grow(len(in))
	pos := 0
	cur := spans[0]
	flush := func(sp span) {
		b.WriteString(in[pos:sp.start])
		b.WriteString(Replacement(sp.kind))
		pos = sp.end
		if s.observe != nil {
			s.observe(sp.kind)
		}
	}
	for _, sp := range spans[1:] {
		if sp.start < cur.end || (sp.start == cur.end && sp.kind == cur.kind) {
			if sp.end > cur.end {
				cur.end = sp.end
			}
			continue
		}
		flush(cur)
		cur = sp
	}
	flush(cur)
	b.WriteString(in[pos:])
	return b.String()
}

// trimTail cuts the end of a text that has itself been cut from something longer, so that no
// fragment of a credential remains: it drops the run of token characters at the end (a key split
// by the cut), and the start of any known-secret pattern still open at the end, in the text and in
// each decoded reading of it (a secret JSON-escaped or percent-encoded is split just the same).
func (s *Scrubber) trimTail(in string) string {
	cut := len(in)
	j := len(in)
	for j > 0 && tailChar(in[j-1]) {
		j--
	}
	if len(in)-j <= 4096 {
		cut = j
	}
	if s.secrets.Len() > 0 {
		cut = min(cut, s.secrets.openStart(in))
		eachView(in, func(v *view) {
			if st := s.secrets.openStart(v.s); st < len(v.s) {
				cut = min(cut, v.pos[st])
			}
		})
	}
	return cutRunes(in, cut)
}

func tailChar(b byte) bool {
	return isAlnum(b) || b == '_' || b == '-' || b == '+' || b == '/' || b == '=' || b == '.' || b == '%'
}
