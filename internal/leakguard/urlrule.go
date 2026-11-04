// SPDX-License-Identifier: Apache-2.0

package leakguard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// URLPolicy is the automatic shape-and-budget rule for outbound URLs and search queries (ADR 0084
// decisions 5 and 6). The zero value of a number means its default; TrustedHosts defaults to
// empty, as the org fetch-trusted host list does.
type URLPolicy struct {
	// TrustedHosts are exempt from the shape rule and the budgets (never from the taint check).
	// A host matches exactly, case-insensitively; wildcard entries are not supported here.
	TrustedHosts []string

	MaxURLBytes        int // default 256
	MaxQueryBytes      int // default 128 (the raw query string, as sent)
	MaxComponentRunes  int // default 31: a component of 32 or more characters is opaque
	RandomMinRunes     int // default 16: shorter components are never judged random
	MaxEncodedEscapes  int // default 2: more %HH escapes of unreserved characters is encoded data
	MaxHostsPerRun     int // default 20 distinct untrusted hosts
	MaxBytesPerHost    int // default 1024 bytes of distinct path and query per host per run
	MaxSearchRunes     int // default 256 characters per search query
	MaxSearchBytesRun  int // default 8192 bytes of search queries per run
	maxDecodeURLRounds int
}

// DefaultURLPolicy returns the defaults, with an empty trusted list.
func DefaultURLPolicy() URLPolicy { return URLPolicy{}.withDefaults() }

func (p URLPolicy) withDefaults() URLPolicy {
	set := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	set(&p.MaxURLBytes, 256)
	set(&p.MaxQueryBytes, 128)
	set(&p.MaxComponentRunes, 31)
	set(&p.RandomMinRunes, 16)
	set(&p.MaxEncodedEscapes, 2)
	set(&p.MaxHostsPerRun, 20)
	set(&p.MaxBytesPerHost, 1024)
	set(&p.MaxSearchRunes, 256)
	set(&p.MaxSearchBytesRun, 8192)
	set(&p.maxDecodeURLRounds, 3)
	return p
}

// RunState holds the budgets one run has used. It is safe for concurrent use. It marshals to
// JSON (MarshalJSON, UnmarshalJSON) so a guard with several replicas keeps it in its database; the
// paths and queries it remembers are SHA-256 digests, not the text.
type RunState struct {
	mu          sync.Mutex
	hosts       map[string]*hostUse
	searchBytes int
}

type hostUse struct {
	bytes int
	seen  map[string]struct{}
}

// NewRunState returns the state of a new run.
func NewRunState() *RunState { return &RunState{hosts: map[string]*hostUse{}} }

type runStateJSON struct {
	Hosts       map[string]hostUseJSON `json:"hosts,omitempty"`
	SearchBytes int                    `json:"searchBytes,omitempty"`
}

type hostUseJSON struct {
	Bytes int      `json:"bytes"`
	Seen  []string `json:"seen"`
}

// MarshalJSON encodes the budgets used.
func (st *RunState) MarshalJSON() ([]byte, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := runStateJSON{Hosts: map[string]hostUseJSON{}, SearchBytes: st.searchBytes}
	for host, h := range st.hosts {
		seen := make([]string, 0, len(h.seen))
		for k := range h.seen {
			seen = append(seen, k)
		}
		slices.Sort(seen)
		out.Hosts[host] = hostUseJSON{Bytes: h.bytes, Seen: seen}
	}
	return json.Marshal(out)
}

// UnmarshalJSON restores the budgets MarshalJSON encoded. Empty input is a new run.
func (st *RunState) UnmarshalJSON(b []byte) error {
	var in runStateJSON
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.hosts = map[string]*hostUse{}
	st.searchBytes = max(in.SearchBytes, 0)
	for host, h := range in.Hosts {
		u := &hostUse{bytes: max(h.Bytes, 0), seen: map[string]struct{}{}}
		for _, k := range h.Seen {
			u.seen[k] = struct{}{}
		}
		st.hosts[host] = u
	}
	return nil
}

// CheckURL applies the rule to an outbound URL of the run and, when it allows the URL to an
// untrusted host, charges the run's budgets.
func (p URLPolicy) CheckURL(st *RunState, raw string) Decision {
	p = p.withDefaults()
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return block(ReasonURLInvalid, "the URL could not be parsed as an absolute http or https URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return block(ReasonURLScheme, "only http and https URLs can be fetched")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return block(ReasonURLInvalid, "the URL has no host")
	}
	if p.trusted(host) {
		return allow(ReasonTrusted)
	}
	if u.User != nil {
		return block(ReasonURLUserinfo, "a URL to a host the org has not trusted cannot carry a user name or password")
	}
	if port := u.Port(); port != "" && port != defaultPort[scheme] {
		return block(ReasonURLPort, "a URL to a host the org has not trusted must use the default port")
	}
	if ipLiteral(host) {
		return block(ReasonURLIPLiteral, "a URL to an IP address is refused unless the org trusts it; use the host name")
	}
	if len(raw) > p.MaxURLBytes {
		return block(ReasonURLLength, "the URL is %d bytes; the limit for hosts the org has not trusted is %d", len(raw), p.MaxURLBytes)
	}
	if len(u.RawQuery) > p.MaxQueryBytes {
		return block(ReasonURLQueryLength, "the query string is %d bytes; the limit for hosts the org has not trusted is %d", len(u.RawQuery), p.MaxQueryBytes)
	}
	if n := encodedUnreserved(u.EscapedPath()) + encodedUnreserved(u.RawQuery); n > p.MaxEncodedEscapes {
		return block(ReasonURLEncoded, "the URL percent-encodes %d characters that need no encoding, which reads as encoded data", n)
	}
	for _, label := range strings.Split(host, ".") {
		if d, bad := p.component(label, ReasonURLOpaque, ReasonURLRandom, "a host name label"); bad {
			return d
		}
	}
	text := p.decodeURLPart(u.EscapedPath()) + " " + p.decodeURLPart(u.RawQuery)
	for _, c := range components(text) {
		if d, bad := p.component(c, ReasonURLOpaque, ReasonURLRandom, "a path or query component"); bad {
			return d
		}
	}
	return st.chargeHost(p, host, u.EscapedPath()+"?"+u.RawQuery)
}

var defaultPort = map[string]string{"http": "80", "https": "443"}

// CheckSearch applies the rule to a web search query of the run and, when it allows it, charges
// the run's search budget. Search providers are not on the trusted list: every query leaves.
func (p URLPolicy) CheckSearch(st *RunState, query string) Decision {
	p = p.withDefaults()
	if n := utf8.RuneCountInString(query); n > p.MaxSearchRunes {
		return block(ReasonSearchLength, "the search query is %d characters; the limit is %d", n, p.MaxSearchRunes)
	}
	for _, c := range components(p.decodeURLPart(query)) {
		if d, bad := p.component(c, ReasonSearchOpaque, ReasonSearchRandom, "a search query word"); bad {
			return d
		}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.searchBytes+len(query) > p.MaxSearchBytesRun {
		return block(ReasonSearchBudget, "the run has sent %d bytes of search queries; the limit is %d per run", st.searchBytes, p.MaxSearchBytesRun)
	}
	st.searchBytes += len(query)
	return allow(ReasonPass)
}

func (p URLPolicy) trusted(host string) bool {
	for _, t := range p.TrustedHosts {
		if strings.TrimSuffix(strings.ToLower(strings.TrimSpace(t)), ".") == host {
			return true
		}
	}
	return false
}

func (st *RunState) chargeHost(p URLPolicy, host, pathQuery string) Decision {
	st.mu.Lock()
	defer st.mu.Unlock()
	h := st.hosts[host]
	if h == nil {
		if len(st.hosts) >= p.MaxHostsPerRun {
			return block(ReasonURLHostBudget, "the run has reached %d hosts the org has not trusted; the limit is %d per run", len(st.hosts), p.MaxHostsPerRun)
		}
		h = &hostUse{seen: map[string]struct{}{}}
	}
	sum := sha256.Sum256([]byte(pathQuery))
	key := hex.EncodeToString(sum[:])
	if _, ok := h.seen[key]; !ok {
		if h.bytes+len(pathQuery) > p.MaxBytesPerHost {
			return block(ReasonURLBytesBudget, "the run has sent %d bytes of paths and queries to this host; the limit is %d per host per run", h.bytes, p.MaxBytesPerHost)
		}
		h.bytes += len(pathQuery)
		h.seen[key] = struct{}{}
	}
	st.hosts[host] = h
	return allow(ReasonPass)
}

// decodeURLPart undoes percent-encoding until nothing changes, at most maxDecodeURLRounds times.
func (p URLPolicy) decodeURLPart(s string) string {
	for range p.maxDecodeURLRounds {
		d, ok := percentDecode(s)
		if !ok {
			break
		}
		s = d
	}
	return s
}

// component judges one component; it returns a blocking decision when the component is opaque
// or random-looking.
func (p URLPolicy) component(c string, opaque, random Reason, what string) (Decision, bool) {
	r := []rune(c)
	if len(r) > p.MaxComponentRunes {
		return block(opaque, "%s is %d characters; the limit for hosts the org has not trusted is %d", what, len(r), p.MaxComponentRunes), true
	}
	if randomLooking(r, p.RandomMinRunes) {
		return block(random, "%s of %d characters looks like encoded or random data", what, len(r)), true
	}
	return Decision{}, false
}

// components splits text into its runs of letters and digits.
func components(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// randomLooking reports whether a component of at least minRunes letters and digits reads as
// encoded or random data rather than a word, a name or a number:
//   - hex (letters a-f and digits, both present), or
//   - mixed letters and digits, or both cases, with frequent changes between lower case, upper
//     case and digits (at least 30% of adjacent pairs) and at least 3 bits of entropy per
//     character.
//
// Words, camel-case names, numbers and dates do not qualify.
func randomLooking(c []rune, minRunes int) bool {
	n := len(c)
	if n < minRunes {
		return false
	}
	const (
		clLower = iota + 1
		clUpper
		clDigit
		clOther
	)
	var lower, upper, digit, changes int
	hex := true
	prev := 0
	counts := map[rune]int{}
	for _, r := range c {
		cl := clOther
		switch {
		case unicode.IsDigit(r):
			cl = clDigit
			digit++
		case unicode.IsUpper(r):
			cl = clUpper
			upper++
		case unicode.IsLower(r):
			cl = clLower
			lower++
		}
		if !isHexRune(r) {
			hex = false
		}
		if prev != 0 && cl != prev {
			changes++
		}
		prev = cl
		counts[r]++
	}
	letters := lower + upper
	if hex && digit > 0 && letters > 0 {
		return true
	}
	if digit == 0 && (upper == 0 || lower == 0) {
		return false
	}
	if float64(changes) < 0.3*float64(n-1) {
		return false
	}
	var h float64
	for _, k := range counts {
		f := float64(k) / float64(n)
		h -= f * math.Log2(f)
	}
	return h >= 3.0
}

func isHexRune(r rune) bool {
	return '0' <= r && r <= '9' || 'a' <= r && r <= 'f' || 'A' <= r && r <= 'F'
}

// encodedUnreserved counts the %HH escapes of characters that never need encoding (letters,
// digits, "-", ".", "_", "~"): a client only writes them to hide data.
func encodedUnreserved(s string) int {
	n := 0
	for i := 0; i+2 < len(s); i++ {
		if s[i] != '%' || !isHex(s[i+1]) || !isHex(s[i+2]) {
			continue
		}
		c := unhex(s[i+1])<<4 | unhex(s[i+2])
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' || c == '%' {
			n++
		}
		i += 2
	}
	return n
}

// ipLiteral reports whether host is an IP address in any form a resolver accepts: dotted or
// IPv6, or labels that are all numbers (decimal, octal or 0x hex, as in "2130706433").
func ipLiteral(host string) bool {
	if net.ParseIP(strings.Trim(host, "[]")) != nil {
		return true
	}
	for _, l := range strings.Split(host, ".") {
		if l == "" {
			return false
		}
		s := l
		if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
			s = s[2:]
			for i := 0; i < len(s); i++ {
				if !isHex(s[i]) {
					return false
				}
			}
			continue
		}
		for i := 0; i < len(s); i++ {
			if s[i] < '0' || s[i] > '9' {
				return false
			}
		}
	}
	return true
}
