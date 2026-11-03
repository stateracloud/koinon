// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"

	"github.com/stateracloud/koinon/internal/sinkclass"
)

// Args is one call's arguments split by ADR 0091 decision 7: the address arguments, which decide
// the sink class and never reach the text match, and the payload, every other string the call
// carries in call order.
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

// MaxArgumentBytes bounds the arguments of one call the guard reads; a larger call is refused.
const MaxArgumentBytes = 1 << 20

// maxNesting bounds the depth of the arguments' JSON.
const maxNesting = 64

var errArgs = errors.New("the arguments must be a JSON object")

// SplitArgs reads a call's arguments (a JSON object, or empty) under declaration d.
func SplitArgs(raw json.RawMessage, d sinkclass.Declaration) (Args, error) {
	return split(raw, d, MaxArgumentBytes)
}

func split(raw json.RawMessage, d sinkclass.Declaration, limit int) (Args, error) {
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
	w := walker{dec: dec, decl: d, out: &out}
	if err := w.object("", 1); err != nil {
		return Args{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Args{}, errArgs
	}
	return out, nil
}

type walker struct {
	dec  *json.Decoder
	decl sinkclass.Declaration
	out  *Args
}

// object reads the members of an object whose '{' was read; path is its dotted name ("" at the top).
func (w walker) object(path string, depth int) error {
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
		if slices.Contains(w.decl.AddressArgs, name) {
			if err := w.address(name); err != nil {
				return err
			}
			continue
		}
		w.out.Payload = append(w.out.Payload, key)
		if err := w.value(name, depth); err != nil {
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
		inner, err := split(json.RawMessage(`{"v":`+string(v)+`}`), sinkclass.Declaration{}, MaxArgumentBytes+16)
		if err != nil {
			return err
		}
		w.out.Payload = append(w.out.Payload, inner.Payload[1:]...)
		return nil
	}
	w.out.Address[name] = s
	if w.decl.Derivation == sinkclass.Fetch {
		if rest := urlPayload(s); rest != "" {
			w.out.Payload = append(w.out.Payload, rest)
		}
	}
	return nil
}

// urlPayload is the part of a fetched URL that is payload: everything after the host (path, query
// and fragment), as sent. A URL that does not parse is payload whole.
func urlPayload(raw string) string {
	s := strings.TrimSpace(raw)
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return s
	}
	i := strings.Index(s, u.Host)
	if i < 0 {
		return s
	}
	return s[i+len(u.Host):]
}

func (w walker) value(path string, depth int) error {
	tok, err := w.dec.Token()
	if err != nil {
		return errArgs
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return w.object(path, depth+1)
		case '[':
			for w.dec.More() {
				if err := w.value(path, depth+1); err != nil {
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
	a, err := split(json.RawMessage(`{"v":`+string(bytes.TrimSpace(raw))+`}`), sinkclass.Declaration{}, MaxResultBytes)
	if err != nil {
		return []string{string(raw)}
	}
	if len(a.Payload) == 0 {
		return nil
	}
	return a.Payload[1:]
}
