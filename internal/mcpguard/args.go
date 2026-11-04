// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode"

	"github.com/stateracloud/koinon/internal/sinkclass"
)

// Args is one call's arguments split by ADR 0091 decision 7: the address arguments, which decide
// the sink class, and the payload, every string the call carries in call order. A derived tool's
// address is not payload, except what of it can carry data: the path, query and fragment of a
// fetched URL, and the whole URL after its scheme when the host is not one the org trusts (its
// labels can carry data too). A fixed tool's address arguments decide nothing, so they are payload.
type Args struct {
	// Address holds the value of each address argument the declaration names, by name. A named
	// argument that is missing or not a string is absent, so a derived class reads it as unparsable.
	Address map[string]string
	// Payload is every other string of the call in the order it was sent: string values, numbers
	// and booleans as written, and the keys of objects (ADR 0084: the wiring passes them all). For a
	// fetch tool the path, query and fragment of its URL are payload too (ADR 0091 decision 3), at
	// the URL argument's place.
	Payload []string
}

// PayloadBytes is what the byte cap counts: the payload as sent, no decoding.
func (a Args) PayloadBytes() int {
	n := 0
	for _, p := range a.Payload {
		n += len(p)
	}
	return n
}

// ErrAmbiguousAddress refuses a call whose address the guard and the server could read differently
// (ADR 0091 decision 7: the class comes from the address, so it must have exactly one reading).
var ErrAmbiguousAddress = errors.New("an address argument of this tool is given more than once, or in a form a server may read differently " +
	"(a repeated key, a key with a dot in it, a different case, or inside a list); send each address argument once, as a plain key")

// MaxArgumentBytes bounds the arguments of one call the guard reads; a larger call is refused.
const MaxArgumentBytes = 1 << 20

// maxNesting bounds the depth of the arguments' JSON.
const maxNesting = 64

var errArgs = errors.New("the arguments must be a JSON object")

// SplitArgs reads a call's arguments (a JSON object, or empty) under declaration d. hosts says which
// fetch hosts the org trusts (nil: none). An ambiguous address is ErrAmbiguousAddress.
func SplitArgs(raw json.RawMessage, d sinkclass.Declaration, hosts sinkclass.HostTrust) (Args, error) {
	return split(raw, d, hosts, MaxArgumentBytes)
}

func split(raw json.RawMessage, d sinkclass.Declaration, hosts sinkclass.HostTrust, limit int) (Args, error) {
	out := Args{Address: map[string]string{}}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return out, nil
	}
	if len(raw) > limit {
		return Args{}, fmt.Errorf("the arguments are larger than %d bytes", limit)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return Args{}, errArgs
	}
	w := walker{dec: dec, decl: d, hosts: hosts, out: &out, addr: map[string]string{}, seen: map[string]int{}}
	for _, a := range d.AddressArgs {
		w.addr[foldKey(a)] = a
	}
	if err := w.object("", 1, false); err != nil {
		return Args{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Args{}, errArgs
	}
	return out, nil
}

type walker struct {
	dec   *json.Decoder
	decl  sinkclass.Declaration
	hosts sinkclass.HostTrust
	out   *Args
	// addr maps the case-folded name of each address argument to its declared name; seen counts
	// the members whose dotted, case-folded name is one of them, at any depth and in any list.
	addr map[string]string
	seen map[string]int
}

// foldKey is a name under Unicode simple case folding: two keys a case-insensitive JSON decoder
// (Go's encoding/json among them) would match to the same field have the same foldKey.
func foldKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		m := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			m = min(m, f)
		}
		b.WriteRune(m)
	}
	return b.String()
}

// object reads the members of an object whose '{' was read; path is its dotted name ("" at the
// top). odd is true below a key that has a dot in it, or inside a list: a name met there is not
// one a server reads as the dotted path.
func (w walker) object(path string, depth int, odd bool) error {
	if depth > maxNesting {
		return errors.New("the arguments are nested too deeply")
	}
	for w.dec.More() {
		tok, err := w.dec.Token()
		if err != nil {
			return errArgs
		}
		key, ok := tok.(string)
		if !ok {
			return errArgs
		}
		name := key
		if path != "" {
			name = path + "." + key
		}
		keyOdd := odd || strings.Contains(key, ".")
		if declared, ok := w.addr[foldKey(name)]; ok {
			// One reading only: the name exactly as declared, once, as plain nested keys. A repeated
			// key (the raw JSON's, which a map would have collapsed), a dotted literal key, another
			// case or a list makes the guard and the server read different addresses.
			w.seen[foldKey(name)]++
			if w.seen[foldKey(name)] > 1 || keyOdd || name != declared {
				return ErrAmbiguousAddress
			}
			if err := w.address(name); err != nil {
				return err
			}
			continue
		}
		w.out.Payload = append(w.out.Payload, key)
		if err := w.value(name, depth, keyOdd); err != nil {
			return err
		}
	}
	_, err := w.dec.Token() // '}'
	return err
}

// address reads the value of an address argument. A string is the address; anything else leaves
// the argument absent (unparsable, so a derived class is external-high) and its content is payload.
func (w walker) address(name string) error {
	var v json.RawMessage
	if err := w.dec.Decode(&v); err != nil {
		return errArgs
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		inner, err := split(json.RawMessage(`{"v":`+string(v)+`}`), sinkclass.Declaration{}, nil, MaxArgumentBytes+16)
		if err != nil {
			return err
		}
		w.out.Payload = append(w.out.Payload, inner.Payload[1:]...)
		return nil
	}
	w.out.Address[name] = s
	switch w.decl.Derivation {
	case sinkclass.Fixed:
		// A fixed class does not depend on the address, so it is data like any other argument.
		w.out.Payload = append(w.out.Payload, s)
	case sinkclass.Fetch:
		if rest := w.urlPayload(s); rest != "" {
			w.out.Payload = append(w.out.Payload, rest)
		}
	}
	return nil
}

// urlPayload is the part of a fetched URL that is payload, as sent. To a host the org trusts it is
// everything after the scheme except the host name: the userinfo, the port, the path, the query and
// the fragment all reach the host and can carry data. To any other host it is everything after the
// scheme: host labels carry data as well as a path does (ADR 0091 decision 8, whatever the
// encoding). A URL that does not parse is payload whole.
func (w walker) urlPayload(raw string) string {
	s := strings.TrimSpace(raw)
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return s
	}
	i := strings.Index(s, "://")
	if i < 0 {
		return s
	}
	rest := s[i+len("://"):]
	host, ok := sinkclass.FetchHost(s)
	if !ok || w.hosts == nil || !w.hosts.TrustsHost(host) {
		return rest
	}
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	authority, tail := rest[:end], rest[end:]
	userinfo, hostport := "", authority
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		userinfo, hostport = authority[:at+1], authority[at+1:]
	}
	port := ""
	if strings.HasPrefix(hostport, "[") {
		if j := strings.Index(hostport, "]"); j >= 0 {
			port = hostport[j+1:]
		}
	} else if j := strings.LastIndex(hostport, ":"); j >= 0 {
		port = hostport[j:]
	}
	return userinfo + port + tail
}

func (w walker) value(path string, depth int, odd bool) error {
	tok, err := w.dec.Token()
	if err != nil {
		return errArgs
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return w.object(path, depth+1, odd)
		case '[':
			for w.dec.More() {
				if err := w.value(path, depth+1, true); err != nil {
					return err
				}
			}
			_, err := w.dec.Token() // ']'
			return err
		}
		return errArgs
	case string:
		w.out.Payload = append(w.out.Payload, t)
	case json.Number:
		w.out.Payload = append(w.out.Payload, t.String())
	case bool:
		w.out.Payload = append(w.out.Payload, fmt.Sprint(t))
	case nil:
	}
	return nil
}

// MaxResultBytes bounds the tool result the taint feed parses as JSON; a larger one, or one that
// is not JSON, is fingerprinted as text whole.
const MaxResultBytes = 8 << 20

// Strings returns every string of a JSON value in order (values, numbers as written and object
// keys): what the taint feed fingerprints of a tool result.
func Strings(raw []byte) []string {
	a, err := split(json.RawMessage(`{"v":`+string(bytes.TrimSpace(raw))+`}`), sinkclass.Declaration{}, nil, MaxResultBytes)
	if err != nil {
		return []string{string(raw)}
	}
	if len(a.Payload) == 0 {
		return nil
	}
	return a.Payload[1:]
}
