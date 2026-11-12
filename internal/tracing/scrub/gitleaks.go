// SPDX-License-Identifier: Apache-2.0

package scrub

import (
	mathbits "math/bits"
	"regexp"
	"strings"
	"sync"

	"github.com/stateracloud/koinon/internal/tracing/scrub/gitleaksrules"
)

// Vendor and credential shapes come from the rule set of gitleaks (MIT; the scanner this
// repository runs in CI), pinned by version in package gitleaksrules, instead of a list we keep
// by hand (issue #56). The set of credential formats is open-ended and changes as vendors add key
// types; the maintained rules follow it. Updating them is a reviewed bump with the generated-sample tests.
//
// A rule is applied the way gitleaks applies it: its keywords are a cheap pre-filter, its regular
// expression finds candidates, the secret group is cut out, a rule's entropy floor (except for the
// fixed vendor shapes of gitleaksNoFloor) and its own and the global allowlists drop false positives. Go's regexp is linear, so no rule backtracks.
//
// What stays ours is what gitleaks does not model for a trace: exact matching of the org's own
// secrets in every encoding, JSON-aware cleaning, header and credential key names, PEM and JWT,
// Koinon's own key shape (kgw_) and the OpenShell placeholder, and the false-positive exemptions.

// gitleaksExcluded lists rules of the pinned set that are not applied, with the reason.
var gitleaksExcluded = map[string]string{
	// Any value after a name containing key, token, secret ... with entropy above 3.5. The scrubber
	// has its own structural rules for credential-named keys (credentialKind, headerKind) that
	// know the exemptions a trace needs (git SHAs, trace IDs, digests, prose); this rule would
	// redact them.
	"generic-api-key": "structural credential-key rules and the exemptions are ours",
}

// gitleaksNoFloor lists the rules of the pinned set that are applied without their entropy floor.
// Each one matches a literal vendor prefix followed by a body of one fixed length, so a match is
// that vendor's key format and not prose or a hash. gitleaks uses the floor to skip placeholders
// in source code; in a trace a real key whose characters happen to repeat (about 2 in 10,000
// AWS key IDs fall under the floor of 3) is still a key, and redacting a placeholder costs little.
// The rules' own allowlists still apply.
//
// TestNoFloorRulesAreFixedVendorShapes checks the list against each gitleaks bump: it fails when a
// listed rule is renamed, dropped, loses its floor or stops being a fixed shape, and when a new
// fixed-shape rule with a floor is not listed.
var gitleaksNoFloor = map[string]struct{}{
	"adobe-client-secret":                 {},
	"alibaba-access-key-id":               {},
	"artifactory-api-key":                 {},
	"aws-access-token":                    {},
	"clickhouse-cloud-api-secret-key":     {},
	"clojars-api-token":                   {},
	"cloudflare-origin-ca-key":            {},
	"digitalocean-access-token":           {},
	"digitalocean-pat":                    {},
	"doppler-api-token":                   {},
	"duffel-api-token":                    {},
	"dynatrace-api-token":                 {},
	"easypost-api-token":                  {},
	"easypost-test-api-token":             {},
	"flutterwave-encryption-key":          {},
	"flutterwave-public-key":              {},
	"flutterwave-secret-key":              {},
	"gcp-api-key":                         {},
	"github-app-token":                    {},
	"github-fine-grained-pat":             {},
	"github-oauth":                        {},
	"github-pat":                          {},
	"github-refresh-token":                {},
	"gitlab-deploy-token":                 {},
	"gitlab-feature-flag-client-token":    {},
	"gitlab-feed-token":                   {},
	"gitlab-incoming-mail-token":          {},
	"gitlab-kubernetes-agent-token":       {},
	"gitlab-oauth-app-secret":             {},
	"gitlab-pat":                          {},
	"gitlab-ptt":                          {},
	"gitlab-rrt":                          {},
	"gitlab-runner-authentication-token":  {},
	"gitlab-scim-token":                   {},
	"gitlab-session-cookie":               {},
	"grafana-service-account-token":       {},
	"heroku-api-key-v2":                   {},
	"huggingface-access-token":            {},
	"huggingface-organization-api-token":  {},
	"infracost-api-token":                 {},
	"intra42-client-secret":               {},
	"linear-api-key":                      {},
	"notion-api-token":                    {},
	"npm-access-token":                    {},
	"octopus-deploy-api-key":              {},
	"openshift-user-token":                {},
	"perplexity-api-key":                  {},
	"postman-api-token":                   {},
	"prefect-api-token":                   {},
	"pulumi-api-token":                    {},
	"readme-api-token":                    {},
	"rubygems-api-token":                  {},
	"scalingo-api-token":                  {},
	"sendgrid-api-token":                  {},
	"sendinblue-api-token":                {},
	"sentry-user-token":                   {},
	"settlemint-application-access-token": {},
	"settlemint-personal-access-token":    {},
	"settlemint-service-access-token":     {},
	"shippo-api-token":                    {},
	"shopify-access-token":                {},
	"shopify-custom-access-token":         {},
	"shopify-private-app-access-token":    {},
	"shopify-shared-secret":               {},
	"slack-config-refresh-token":          {},
}

// ownedShape is a credential shape the scrubber matches itself, because the pinned gitleaks set
// has no rule for it or its rule is narrower than what a trace must cover. The table is the whole
// list: every entry carries the reason it exists and a sample, and TestOwnedShapes checks each
// one (the sample is redacted and gitleaks must not match it, so a bump that adds an upstream rule
// fails the test and the entry is deleted). It is not a place to
// add the next vendor: send that rule to gitleaks (exit condition of issue #212).
type ownedShape struct {
	name   string
	prefix string // literal every match starts with; the cheap pre-filter
	// re is anchored at the start: it is run on the text from each occurrence of prefix whose left
	// side passes leftOK, so a word that merely ends in the prefix (task-, risk-) costs no scan.
	re *regexp.Regexp
	// minEntropy, when above zero, drops a match whose Shannon entropy is not above it (a
	// placeholder such as sk-xxxx...), as the floor of the gitleaks OpenAI rule does.
	minEntropy float64
	// reason is why gitleaks' rule does not do (issue #212, maintainer decision 2026-11-11).
	reason string
}

var ownedShapes = []ownedShape{
	{
		name:   "openrouter",
		prefix: "sk-or-v1-",
		re:     regexp.MustCompile(`^sk-or-v1-[A-Za-z0-9]{8,}`),
		reason: "OpenRouter is a provider Koinon routes to and gitleaks has no rule for its key",
	},
	{
		name:   "anthropic-truncated",
		prefix: "sk-ant-",
		re:     regexp.MustCompile(`^sk-ant-[A-Za-z0-9_-]{8,}`),
		reason: "a cut-off or mistyped Anthropic key is as sensitive as a whole one, and gitleaks only knows the full 108-character shape",
	},
	{
		name:   "openai-legacy",
		prefix: "sk-",
		re:     regexp.MustCompile(`^sk-[A-Za-z0-9]{20,}`),
		// gitleaks' OpenAI rule knows the whole legacy key with its T3BlbkFJ marker and a
		// terminator, and the newer prefixed keys.
		reason:     "a legacy sk- key that is cut off, unterminated or without the T3BlbkFJ marker (other providers issue sk-<hex> keys) is as sensitive as a whole one",
		minEntropy: 3,
	},
	{
		name:   "google-oauth-token",
		prefix: "ya29.",
		re:     regexp.MustCompile(`ya29\.[A-Za-z0-9_-]{20,}`),
		reason: "Google OAuth access token; gitleaks has no rule",
	},
	{
		name:   "google-oauth-client-secret",
		prefix: "GOCSPX-",
		re:     regexp.MustCompile(`^GOCSPX-[A-Za-z0-9_-]{20,}`),
		reason: "Google OAuth client secret; gitleaks has no rule",
	},
}

// findOwned appends the spans of the owned shapes in in.
func findOwned(in string, spans []span) []span {
	for i := range ownedShapes {
		o := &ownedShapes[i]
		for from := 0; ; {
			j := strings.Index(in[from:], o.prefix)
			if j < 0 {
				break
			}
			at := from + j
			from = at + 1
			if !leftOK(in, at) {
				continue
			}
			m := o.re.FindStringIndex(in[at:])
			if m == nil {
				continue
			}
			if o.minEntropy > 0 && entropy(in[at:at+m[1]]) <= o.minEntropy {
				continue
			}
			spans = append(spans, span{at, at + m[1], KindAPIKey})
			from = at + m[1]
		}
	}
	return spans
}

// glRule is a compiled gitleaks rule.
type glRule struct {
	id      string
	re      *regexp.Regexp
	group   int
	entropy float64
	allow   []glAllow
}

// glAllow is a compiled allowlist.
type glAllow struct {
	and     bool
	target  string
	regexes []*regexp.Regexp
	stops   []string // lower-case
	never   bool     // an AND allowlist with a path or commit criterion
}

// glSet is the compiled rule set.
type glSet struct {
	rules   []glRule
	global  []glAllow
	skipped []string // rules that did not compile; a test requires none
	kw      *kwMatcher
}

var (
	glOnce sync.Once
	glAll  *glSet
)

// gitleaks returns the compiled rule set, built on first use (about a millisecond per ten rules,
// so a process that never scrubs content does not pay for it).
func gitleaks() *glSet {
	glOnce.Do(func() {
		glAll = compileGitleaks()
	})
	return glAll
}

func compileGitleaks() *glSet {
	set, err := gitleaksrules.Load()
	if err != nil {
		panic(err) // the embedded file is checked by a test
	}
	g := &glSet{}
	for _, a := range set.Global {
		ca, ok := compileAllow(a)
		if !ok {
			g.skipped = append(g.skipped, "global allowlist")
			continue
		}
		g.global = append(g.global, ca)
	}
	var kws [][]string
	for _, r := range set.Rules {
		if _, skip := gitleaksExcluded[r.ID]; skip {
			continue
		}
		re, err := regexp.Compile(r.Regex)
		if err != nil || r.SecretGroup > re.NumSubexp() {
			g.skipped = append(g.skipped, r.ID)
			continue
		}
		cr := glRule{id: r.ID, re: re, group: r.SecretGroup, entropy: r.Entropy}
		if _, ok := gitleaksNoFloor[r.ID]; ok {
			cr.entropy = 0
		}
		for _, a := range r.Allowlists {
			ca, ok := compileAllow(a)
			if !ok {
				g.skipped = append(g.skipped, r.ID)
				cr.re = nil
				break
			}
			cr.allow = append(cr.allow, ca)
		}
		if cr.re == nil {
			continue
		}
		g.rules = append(g.rules, cr)
		kws = append(kws, r.Keywords)
	}
	g.kw = newKwMatcher(kws)
	return g
}

func compileAllow(a gitleaksrules.Allowlist) (glAllow, bool) {
	out := glAllow{and: a.MatchCondition == "AND", target: a.RegexTarget}
	// A trace has no path and no commit. In an AND allowlist such a criterion can never hold, so
	// the allowlist never matches; in an OR allowlist it simply adds nothing.
	if out.and && (a.HasPaths || a.HasCommits) {
		out.never = true
	}
	for _, s := range a.Regexes {
		re, err := regexp.Compile(s)
		if err != nil {
			return out, false
		}
		out.regexes = append(out.regexes, re)
	}
	for _, w := range a.StopWords {
		out.stops = append(out.stops, strings.ToLower(w))
	}
	return out, true
}

// allows reports whether the allowlist drops a finding with this secret, whole match and line.
func (a *glAllow) allows(secret, match, line string) bool {
	if a.never {
		return false
	}
	target := secret
	switch a.target {
	case "match":
		target = match
	case "line":
		target = line
	}
	var hitRe, hitStop bool
	for _, re := range a.regexes {
		if re.MatchString(target) {
			hitRe = true
			break
		}
	}
	if len(a.stops) > 0 {
		low := strings.ToLower(secret)
		for _, w := range a.stops {
			if strings.Contains(low, w) {
				hitStop = true
				break
			}
		}
	}
	if a.and {
		return (len(a.regexes) == 0 || hitRe) && (len(a.stops) == 0 || hitStop) && (len(a.regexes) > 0 || len(a.stops) > 0)
	}
	return hitRe || hitStop
}

// findGitleaks appends the spans of the gitleaks rules whose keywords occur in in. The span is
// the secret, not the surrounding key name, so the trace keeps its context.
func findGitleaks(in string, spans []span) []span {
	spans = findOwned(in, spans)
	g := gitleaks()
	var small [8]uint64
	cand := g.kw.scan(in, small[:0])
	for w, bits := range cand {
		for bits != 0 {
			i := w*64 + mathbits.TrailingZeros64(bits)
			bits &= bits - 1
			spans = g.rules[i].find(g, in, spans)
		}
	}
	return spans
}

func (r *glRule) find(g *glSet, in string, spans []span) []span {
	for _, m := range r.re.FindAllStringSubmatchIndex(in, -1) {
		a, b := m[0], m[1]
		if r.group > 0 {
			a, b = m[2*r.group], m[2*r.group+1]
		} else {
			for i := 1; 2*i+1 < len(m); i++ {
				if m[2*i] >= 0 && m[2*i] != m[2*i+1] {
					a, b = m[2*i], m[2*i+1]
					break
				}
			}
		}
		if a < 0 || a >= b {
			continue
		}
		secret := in[a:b]
		if r.entropy > 0 && entropy(secret) <= r.entropy {
			continue
		}
		if r.allowed(g, in, m[0], m[1], secret) {
			continue
		}
		spans = append(spans, span{a, b, KindAPIKey})
	}
	return spans
}

func (r *glRule) allowed(g *glSet, in string, ms, me int, secret string) bool {
	match := in[ms:me]
	var line string
	lineOf := func() string {
		if line == "" {
			s := strings.LastIndexByte(in[:ms], '\n') + 1
			e := len(in)
			if i := strings.IndexByte(in[me:], '\n'); i >= 0 {
				e = me + i
			}
			line = in[s:e]
		}
		return line
	}
	for _, list := range [2][]glAllow{r.allow, g.global} {
		for i := range list {
			a := &list[i]
			l := ""
			if a.target == "line" {
				l = lineOf()
			}
			if a.allows(secret, match, l) {
				return true
			}
		}
	}
	return false
}

// kwMatcher finds, in one pass over a text, which rules have a keyword in it: an Aho-Corasick
// automaton over the lower-cased keywords, run as a DFA over byte classes (bytes that occur in no
// keyword share class 0, so the table stays small). Matching is ASCII case-insensitive and does
// not allocate.
type kwMatcher struct {
	class   [256]uint8
	classes int
	next    []int32   // state*classes + class -> state
	out     [][]int32 // state -> rules with a keyword ending here, through the failure chain
	words   int       // length of the result bitset in words
	always  []int     // rules without keywords: they run on every text
}

func newKwMatcher(kws [][]string) *kwMatcher {
	m := &kwMatcher{words: (len(kws) + 63) / 64}
	// Byte classes: one per distinct byte of any keyword.
	m.classes = 1
	for _, ks := range kws {
		for _, k := range ks {
			for i := 0; i < len(k); i++ {
				c := lowerByte(k[i])
				if m.class[c] == 0 {
					m.class[c] = uint8(m.classes) //nolint:gosec // bounded: more than 64 classes panics below
					m.classes++
				}
			}
		}
	}
	for b := 0; b < 256; b++ {
		m.class[b] = m.class[lowerByte(byte(b))]
	}
	// Trie.
	type node struct {
		child [64]int32
		fail  int32
		out   []int32
	}
	if m.classes > 64 {
		panic("scrub: too many keyword byte classes")
	}
	nodes := []node{{}}
	for ri, ks := range kws {
		if len(ks) == 0 {
			m.always = append(m.always, ri)
			continue
		}
		for _, k := range ks {
			cur := int32(0)
			for i := 0; i < len(k); i++ {
				c := m.class[k[i]]
				if nodes[cur].child[c] == 0 {
					nodes = append(nodes, node{})
					nodes[cur].child[c] = int32(len(nodes) - 1) //nolint:gosec // a trie of a few hundred keywords is far below int32
				}
				cur = nodes[cur].child[c]
			}
			nodes[cur].out = append(nodes[cur].out, int32(ri))
		}
	}
	// Failure links and the full transition table, breadth first.
	queue := make([]int32, 0, len(nodes))
	for c := 1; c < m.classes; c++ {
		if ch := nodes[0].child[c]; ch != 0 {
			queue = append(queue, ch)
		}
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		nodes[n].out = append(nodes[n].out, nodes[nodes[n].fail].out...)
		for c := 1; c < m.classes; c++ {
			ch := nodes[n].child[c]
			f := nodes[nodes[n].fail].child[c]
			if ch == 0 {
				nodes[n].child[c] = f
				continue
			}
			nodes[ch].fail = f
			queue = append(queue, ch)
		}
	}
	m.next = make([]int32, len(nodes)*m.classes)
	m.out = make([][]int32, len(nodes))
	for i := range nodes {
		copy(m.next[i*m.classes:(i+1)*m.classes], nodes[i].child[:m.classes])
		m.out[i] = nodes[i].out
	}
	return m
}

func lowerByte(b byte) byte {
	if 'A' <= b && b <= 'Z' {
		return b + 'a' - 'A'
	}
	return b
}

// scan returns the bitset of rules that have a keyword in in (and the rules without keywords).
// buf is scratch space with room for the bitset.
func (m *kwMatcher) scan(in string, buf []uint64) []uint64 {
	var bits []uint64
	if m.words <= cap(buf) {
		bits = buf[:m.words]
		clear(bits)
	} else {
		bits = make([]uint64, m.words)
	}
	for _, r := range m.always {
		bits[r/64] |= 1 << (r % 64)
	}
	state := int32(0)
	classes := int32(m.classes) //nolint:gosec // at most 64, checked in newKwMatcher
	for i := 0; i < len(in); i++ {
		state = m.next[state*classes+int32(m.class[in[i]])]
		if o := m.out[state]; o != nil {
			for _, r := range o {
				bits[r/64] |= 1 << (r % 64)
			}
		}
	}
	return bits
}
