// SPDX-License-Identifier: Apache-2.0

// Package mcptoolpin is the one definition of what an MCP tool is to the model, and of the hash an
// admin approves (#1132, ADR 0097 step 2, ADR 0091 decision 4).
//
// A tool list is reviewed configuration, not session data. The text of a tool (its name, title,
// description, annotations and schemas) reaches the model as instructions, so Koinon serves a tool
// only when the hash of its sanitised text equals the hash an admin approved. The sync (the prober)
// and the gateway both build the tool with [Normalise] and compare with [Tool.Hash], so they cannot
// disagree about what a change is.
//
// The hash covers every field the model sees. It does not cover _meta, icons or any field a server
// invents: [Tool.JSON] does not serve them either. It is not a fingerprint of the server: a new
// field that the model never sees is not a change.
package mcptoolpin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/stateracloud/koinon/internal/mcpsanitize"
)

const (
	// MaxTools is the most tools Koinon reads from one server in one list. A tool past it is left out,
	// never passed unchecked.
	MaxTools = 256
	// MaxToolBytes is the most bytes of one tool's JSON that the gateway reads. A longer tool is left
	// out.
	MaxToolBytes = 256 << 10
	// MaxTitle is the cap on a tool's title in characters.
	MaxTitle = 200

	hashDomain = "koinon-tool-pin/v1"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// ErrUnusable says a tool cannot be served at all: its name is not one Koinon routes, or it is too big.
var ErrUnusable = errors.New("mcptoolpin: the tool is unusable")

// Published is one entry of a server's tools/list as the server sent it, with only the members the
// model sees. Other members are dropped when it is decoded.
type Published struct {
	Name         string          `json:"name"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	Annotations  json.RawMessage `json:"annotations"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
}

// Annotations are the hints a server gives about a tool. The model sees them. Only these members are
// kept; a server's own hint never sets a tier (ADR 0057).
type Annotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// Tool is a tool after sanitising: the text Koinon serves and hashes.
type Tool struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	Annotations  *Annotations    `json:"annotations,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

// Normalise sanitises a published tool. The report says whether any text was cut or cleaned. An error
// (ErrUnusable) means the tool cannot be served and the caller leaves it out.
func Normalise(p Published) (Tool, mcpsanitize.Report, error) {
	var rep mcpsanitize.Report
	if !namePattern.MatchString(p.Name) {
		return Tool{}, rep, ErrUnusable
	}
	t := Tool{Name: p.Name}
	var r mcpsanitize.Report
	t.Description, r = mcpsanitize.Description(p.Description)
	rep = merge(rep, r)
	t.Title, r = mcpsanitize.Text(p.Title, MaxTitle)
	rep = merge(rep, r)
	if a := parseAnnotations(p.Annotations); a != nil {
		a.Title, r = mcpsanitize.Text(a.Title, MaxTitle)
		rep = merge(rep, r)
		t.Annotations = a
	}
	t.InputSchema = objectSchema(p.InputSchema, true)
	t.InputSchema, r = mcpsanitize.Schema(t.InputSchema)
	rep = merge(rep, r)
	if out := objectSchema(p.OutputSchema, false); out != nil {
		t.OutputSchema, r = mcpsanitize.Schema(out)
		rep = merge(rep, r)
	}
	t.InputSchema = canonical(t.InputSchema)
	t.OutputSchema = canonical(t.OutputSchema)
	return t, rep, nil
}

func merge(a, b mcpsanitize.Report) mcpsanitize.Report {
	a.OriginalLen = max(a.OriginalLen, b.OriginalLen)
	a.Capped = a.Capped || b.Capped
	a.Cleaned = a.Cleaned || b.Cleaned
	return a
}

// objectSchema returns raw when it is a JSON object. A schema that is missing or not an object is the
// empty object schema for an input (MCP requires an object) and nil for an output.
func objectSchema(raw json.RawMessage, input bool) json.RawMessage {
	t := bytes.TrimSpace(raw)
	if len(t) > 0 && t[0] == '{' && json.Valid(t) {
		return t
	}
	if input {
		return json.RawMessage(`{"type":"object"}`)
	}
	return nil
}

func parseAnnotations(raw json.RawMessage) *Annotations {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] != '{' {
		return nil
	}
	var a Annotations
	if json.Unmarshal(t, &a) != nil {
		return nil
	}
	if a == (Annotations{}) {
		return nil
	}
	return &a
}

// canonical re-encodes a JSON document with sorted keys and no insignificant space. Numbers keep
// their text. A document that does not decode is returned as it is.
func canonical(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return raw
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// JSON is the canonical form of the tool. It is what the pin stores and what the gateway serves, and
// the input of Hash.
func (t Tool) JSON() json.RawMessage {
	b, err := json.Marshal(t)
	if err != nil {
		return nil // cannot happen: every member is a string or valid JSON
	}
	return canonical(b)
}

// Hash is the hex SHA-256 of the tool's canonical JSON under a fixed domain. Equal hashes mean the
// model reads equal text.
func (t Tool) Hash() string {
	h := sha256.New()
	h.Write([]byte(hashDomain))
	h.Write([]byte{0})
	h.Write(t.JSON())
	return hex.EncodeToString(h.Sum(nil))
}

// Parse reads a stored tool (the output of JSON). It does not sanitise: stored text was sanitised.
func Parse(raw json.RawMessage) (Tool, error) {
	var t Tool
	if err := json.Unmarshal(raw, &t); err != nil {
		return Tool{}, err
	}
	if !namePattern.MatchString(t.Name) {
		return Tool{}, ErrUnusable
	}
	return t, nil
}

// HashPattern is the shape of a stored hash.
const HashPattern = `^[0-9a-f]{64}$`
