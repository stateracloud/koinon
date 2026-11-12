// SPDX-License-Identifier: Apache-2.0

// Package sinkclass is the vocabulary of ADR 0091: the sink class of a tool call (where its data
// goes), the per-tool declaration that fixes the class or says how to derive it from the call's
// address arguments, and the derivation itself. The leak guard (old issue 349) calls Classify for every tools/call;
// the MCP server records (old issue 350) store the declarations.
//
// The cautious reading runs through the package: an unclassified tool, an address argument that does
// not parse, a repository the org's scope does not list, a public one, one of unknown visibility and
// one whose visibility reading is stale are all ExternalHigh.
package sinkclass

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Class is the sink class of one tool call (ADR 0091 decision 1).
type Class string

const (
	// None reads only; nothing leaves.
	None Class = "none"
	// Internal writes only into a system the org owns and does not publish.
	Internal Class = "internal"
	// ExternalLow is a small payload in an address or query to an outside party.
	ExternalLow Class = "external-low"
	// ExternalHigh is free text to a party outside the org. It is the class of everything unknown.
	ExternalHigh Class = "external-high"
)

// Classes are the four classes, from the least to the most exposed.
var Classes = []Class{None, Internal, ExternalLow, ExternalHigh}

// Valid reports whether c is one of the four classes.
func (c Class) Valid() bool { return slices.Contains(Classes, c) }

// Derivation says how a tool's class is decided.
type Derivation string

const (
	// Fixed: the declaration's class, whatever the arguments.
	Fixed Derivation = "fixed"
	// CodeHost: per call from the owner and repo address arguments and the org's code-host scope.
	CodeHost Derivation = "code_host"
	// Fetch: per call from the url address argument and the org's fetch-trusted hosts.
	Fetch Derivation = "fetch"
)

// Derivations are the three derivations.
var Derivations = []Derivation{Fixed, CodeHost, Fetch}

// MaxAddressArgs bounds the address arguments of a fixed declaration.
const MaxAddressArgs = 8

// InvalidError is a declaration that does not validate. Its text is shown to the admin.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &InvalidError{Msg: fmt.Sprintf(format, a...)} }

var argName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.\-]{0,63}$`)

// Declaration is what a tool says about its calls (ADR 0091 decision 2).
type Declaration struct {
	// Class is the fixed class. For a derived tool it is ExternalHigh, the class of a call whose
	// address arguments cannot be parsed.
	Class Class `json:"sinkClass"`
	// Derivation is Fixed, CodeHost or Fetch.
	Derivation Derivation `json:"derivation"`
	// AddressArgs name the arguments that say where a call goes, in the order the derivation reads
	// them: CodeHost takes [owner, repo], Fetch takes [url]. Every other argument is payload. A fixed
	// tool may name address arguments too: they are shown to the approver, and since they decide
	// nothing they are payload as well (the byte cap and the text match count them).
	AddressArgs []string `json:"addressArgs"`
	// ResultUntrustedInput and ResultPrivateData say what the tool's results carry for the session
	// legs. nil is the server's own leg.
	ResultUntrustedInput *bool `json:"resultUntrustedInput,omitempty"`
	ResultPrivateData    *bool `json:"resultPrivateData,omitempty"`
}

// Default is the declaration of a tool nobody classified: external-high, the server's legs. It is
// also what a changed server URL or tool content hash puts a tool back to.
func Default() Declaration {
	return Declaration{Class: ExternalHigh, Derivation: Fixed, AddressArgs: []string{}}
}

// IsDefault reports whether d is the declaration Default returns.
func (d Declaration) IsDefault() bool {
	return d.Class == ExternalHigh && d.Derivation == Fixed && len(d.AddressArgs) == 0 &&
		d.ResultUntrustedInput == nil && d.ResultPrivateData == nil
}

// Equal reports whether two declarations say the same.
func (d Declaration) Equal(o Declaration) bool {
	return d.Class == o.Class && d.Derivation == o.Derivation && slices.Equal(d.AddressArgs, o.AddressArgs) &&
		boolPtrEqual(d.ResultUntrustedInput, o.ResultUntrustedInput) && boolPtrEqual(d.ResultPrivateData, o.ResultPrivateData)
}

// ResultLegs are the legs a result of the tool sets: the tool's own declaration where it has one,
// else its server's (ADR 0091 decision 4). The guard and the template legs test both call it, so
// the per-leg fallback cannot drift between them.
func (d Declaration) ResultLegs(serverUntrustedInput, serverPrivateData bool) (untrustedInput, privateData bool) {
	untrustedInput, privateData = serverUntrustedInput, serverPrivateData
	if d.ResultUntrustedInput != nil {
		untrustedInput = *d.ResultUntrustedInput
	}
	if d.ResultPrivateData != nil {
		privateData = *d.ResultPrivateData
	}
	return untrustedInput, privateData
}

func boolPtrEqual(a, b *bool) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// Normalize validates d and returns it in stored form. A derived declaration has class ExternalHigh
// whatever it came with, because the class column is only its fallback; an empty class on a fixed
// declaration is an error, not a default, so a client that forgot it does not silently classify a tool.
func (d Declaration) Normalize() (Declaration, error) {
	if !slices.Contains(Derivations, d.Derivation) {
		return Declaration{}, invalid("derivation must be one of fixed, code_host, fetch")
	}
	out := d
	out.AddressArgs = slices.Clone(d.AddressArgs)
	if out.AddressArgs == nil {
		out.AddressArgs = []string{}
	}
	switch d.Derivation {
	case Fixed:
		if !d.Class.Valid() {
			return Declaration{}, invalid("sink class must be one of none, internal, external-low, external-high")
		}
		if len(out.AddressArgs) > MaxAddressArgs {
			return Declaration{}, invalid("a tool names at most %d address arguments", MaxAddressArgs)
		}
	case CodeHost:
		if len(out.AddressArgs) != 2 {
			return Declaration{}, invalid("a code_host tool names its owner and repo arguments, in that order")
		}
		out.Class = ExternalHigh
	case Fetch:
		if len(out.AddressArgs) != 1 {
			return Declaration{}, invalid("a fetch tool names its url argument")
		}
		out.Class = ExternalHigh
	}
	for i, a := range out.AddressArgs {
		if !argName.MatchString(a) {
			return Declaration{}, invalid("address argument %q is not an argument name (letters, digits, _ . -, at most 64)", a)
		}
		if slices.Contains(out.AddressArgs[:i], a) {
			return Declaration{}, invalid("address argument %q is listed twice", a)
		}
	}
	return out, nil
}

// Visibility is a repository's visibility as the code-host connector last read it.
type Visibility string

const (
	VisPrivate  Visibility = "private"
	VisInternal Visibility = "internal"
	VisPublic   Visibility = "public"
	VisUnknown  Visibility = "unknown"
)

// DefaultMaxAge is how old a visibility reading may be and still count (see Scope). A repository
// that became public after the reading would be a hole, so the window is short; an hour matches the
// connector's refresh.
const DefaultMaxAge = time.Hour

// MaxClockSkew is how far a reading may be dated after the reader's clock and still count as fresh.
// A reading further ahead is treated as stale on read and refused on write.
const MaxClockSkew = time.Minute

// Repo is one repository of the org's code-host scope: a repository of one connection. Connection
// is the connection's ID. The same owner and name on another connection is another repository (it
// may be on another host).
type Repo struct {
	Connection  string
	Owner, Name string
	Visibility  Visibility
	CheckedAt   time.Time
}

// Scope is the org's code-host scope: the repositories of each code-host connection with their
// visibility (ADR 0091 decision 3). A lookup names the connection; the repositories of the other
// connections are not consulted.
type Scope struct {
	repos  map[string][]Repo
	maxAge time.Duration
}

// NewScope builds a scope. maxAge <= 0 means DefaultMaxAge. Owner and repository names compare
// without case, as the code hosts do.
func NewScope(repos []Repo, maxAge time.Duration) Scope {
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	s := Scope{repos: map[string][]Repo{}, maxAge: maxAge}
	for _, r := range repos {
		k := key(r.Connection, r.Owner, r.Name)
		s.repos[k] = append(s.repos[k], r)
	}
	return s
}

func key(connection, owner, repo string) string {
	return connection + "/" + strings.ToLower(owner) + "/" + strings.ToLower(repo)
}

// Internal reports whether owner/repo is a repository the org owns and does not publish: the
// connection lists it, and its reading is private or internal and no older than the scope's maxAge.
// Unknown visibility, a public repository, a stale reading, a reading dated in the future (a writer
// error: it would stay fresh forever), no connection, a repository only another connection lists
// and a repository the scope does not list are all not internal.
func (s Scope) Internal(connection, owner, repo string, now time.Time) bool {
	if connection == "" {
		return false
	}
	rs := s.repos[key(connection, owner, repo)]
	if len(rs) == 0 {
		return false
	}
	for _, r := range rs {
		if r.Visibility != VisPrivate && r.Visibility != VisInternal {
			return false
		}
		if age := now.Sub(r.CheckedAt); age > s.maxAge || age < -MaxClockSkew {
			return false
		}
	}
	return true
}

// HostTrust says which hosts the org's fetch rule exempts (orgsettings.LeakGuard).
type HostTrust interface{ TrustsHost(host string) bool }

// Classify returns the sink class of one call. address holds the values of the call's address
// arguments by name; the payload arguments are not an input, they never decide the class. A derived
// tool whose address arguments are missing or do not parse is ExternalHigh. connection is the ID of
// the code-host connection the tool's server is linked to (mcp_servers.code_host_connection_id), "" for
// none: a code-host tool of an unlinked server is ExternalHigh, and the repository is looked up in
// that connection's list only.
func Classify(d Declaration, address map[string]string, connection string, scope Scope, hosts HostTrust, now time.Time) Class {
	switch d.Derivation {
	case Fixed:
		if d.Class.Valid() {
			return d.Class
		}
		return ExternalHigh
	case CodeHost:
		if len(d.AddressArgs) != 2 {
			return ExternalHigh
		}
		owner, repo := address[d.AddressArgs[0]], address[d.AddressArgs[1]]
		if !repoPart(owner) || !repoPart(repo) {
			return ExternalHigh
		}
		if scope.Internal(connection, owner, repo, now) {
			return Internal
		}
		return ExternalHigh
	case Fetch:
		if len(d.AddressArgs) != 1 {
			return ExternalHigh
		}
		host, ok := FetchHost(address[d.AddressArgs[0]])
		if !ok {
			return ExternalHigh
		}
		if hosts != nil && hosts.TrustsHost(host) {
			return Internal
		}
		return ExternalLow
	}
	return ExternalHigh
}

// ValidRepoPart reports whether s is an owner or repository name the derivation accepts, and the
// scope's writer stores.
func ValidRepoPart(s string) bool { return repoPart(s) }

// repoPart accepts one path segment of a repository address: ASCII only (a code host's names are,
// and a non-ASCII letter such as U+212A lower-cases to an ASCII one, so it would match a different
// repository), no separator, no space, no control character.
func repoPart(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f || c == '/' || c == '\\' {
			return false
		}
	}
	return true
}

// FetchHost returns the host of an http or https URL, or false for anything else: a URL without a
// host, another scheme, or text that does not parse. The path, query and fragment are payload.
func FetchHost(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	h := strings.ToLower(u.Hostname())
	if h == "" {
		return "", false
	}
	return h, true
}
