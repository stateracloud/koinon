// SPDX-License-Identifier: Apache-2.0

package export

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Preset is a backend's way of authenticating and of naming its project. Every backend takes OTLP/HTTP
// protobuf; a preset only adds the headers it needs (ADR 0074, decision 8).
type Preset string

// The presets of v1.
const (
	Langfuse  Preset = "langfuse"
	Datadog   Preset = "datadog"
	LangSmith Preset = "langsmith"
	Opik      Preset = "opik"
	Generic   Preset = "generic"
)

// Presets lists every preset.
func Presets() []Preset { return []Preset{Langfuse, Datadog, LangSmith, Opik, Generic} }

// ParsePreset returns the preset named s.
func ParsePreset(s string) (Preset, error) {
	p := Preset(s)
	if !slices.Contains(Presets(), p) {
		return "", &InvalidError{Msg: fmt.Sprintf("backend must be one of langfuse, datadog, langsmith, opik, generic; got %q", s)}
	}
	return p, nil
}

// Field is one input of a preset.
type Field struct {
	Name     string
	Secret   bool
	Required bool
}

// Fields returns the inputs of a preset. A generic destination has no fixed fields: every secret is a
// header, named by its key.
func Fields(p Preset) []Field {
	switch p {
	case Langfuse:
		return []Field{{"public_key", true, true}, {"secret_key", true, true}}
	case Datadog:
		return []Field{{"api_key", true, true}}
	case LangSmith:
		return []Field{{"api_key", true, true}, {"project", false, false}}
	case Opik:
		return []Field{{"api_key", true, true}, {"workspace", false, true}, {"project", false, false}}
	}
	return nil
}

// InvalidError is a destination that cannot be used as entered. Msg is safe to show and holds no
// secret value.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &InvalidError{Msg: fmt.Sprintf(format, a...)} }

var headerName = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]{1,128}$`)

// reserved are headers the client sets itself or that change how the request is framed.
var reserved = map[string]bool{
	"host": true, "content-length": true, "content-type": true, "content-encoding": true, "transfer-encoding": true,
	"connection": true, "te": true, "trailer": true, "upgrade": true, "expect": true, "proxy-authorization": true,
	"accept": true, "user-agent": true,
}

// Destination is everything the client needs to send to one backend. Secrets hold values, so a
// Destination must never be logged; String and GoString hide them.
type Destination struct {
	Preset   Preset
	Endpoint string
	// Params are the preset's inputs that are not secret (project, workspace).
	Params map[string]string
	// Secrets are the preset's secret inputs, or, for Generic, header name to value.
	Secrets map[string]string
}

func (d Destination) String() string {
	return fmt.Sprintf("trace destination %s %s ([secrets redacted])", d.Preset, redactURL(d.Endpoint))
}

// GoString implements fmt.GoStringer.
func (d Destination) GoString() string { return d.String() }

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid url)"
	}
	return u.Scheme + "://" + u.Host + u.EscapedPath()
}

// ValidateEndpoint checks the endpoint's shape. Whether its host is allowed is the egress policy's
// call, made on save by the service and again on every dial by the client.
func ValidateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "":
		return invalid("endpoint must be an http(s) URL, for example https://cloud.langfuse.com/api/public/otel/v1/traces")
	case u.User != nil:
		return invalid("endpoint must not carry credentials; put them in the secret fields")
	case u.Fragment != "":
		return invalid("endpoint must not have a fragment")
	case len(raw) > 2048:
		return invalid("endpoint is longer than 2048 characters")
	}
	return nil
}

// Validate checks the destination: the endpoint, the preset's fields, and that no field is unknown.
func (d Destination) Validate() error {
	if _, err := ParsePreset(string(d.Preset)); err != nil {
		return err
	}
	if err := ValidateEndpoint(d.Endpoint); err != nil {
		return err
	}
	if d.Preset == Generic {
		if len(d.Params) > 0 {
			return invalid("a generic destination has no settings besides its headers")
		}
		if len(d.Secrets) == 0 {
			return invalid("a generic destination needs at least one header")
		}
		for _, name := range sortedKeys(d.Secrets) {
			if !headerName.MatchString(name) {
				return invalid("%q is not a valid header name", name)
			}
			if reserved[strings.ToLower(name)] {
				return invalid("the header %q is set by Koinon and cannot be given", name)
			}
			if err := validValue(name, d.Secrets[name]); err != nil {
				return err
			}
		}
		return nil
	}
	known := map[string]Field{}
	for _, f := range Fields(d.Preset) {
		known[f.Name] = f
		have := d.Params
		if f.Secret {
			have = d.Secrets
		}
		if v := have[f.Name]; v == "" {
			if f.Required {
				return invalid("%s needs %s", d.Preset, f.Name)
			}
		} else if err := validValue(f.Name, v); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(d.Params) {
		if f, ok := known[name]; !ok || f.Secret {
			return invalid("%s has no setting %q", d.Preset, name)
		}
	}
	for _, name := range sortedKeys(d.Secrets) {
		if f, ok := known[name]; !ok || !f.Secret {
			return invalid("%s has no secret %q", d.Preset, name)
		}
	}
	return nil
}

func validValue(name, v string) error {
	if len(v) > 4096 {
		return invalid("%s is longer than 4096 characters", name)
	}
	for _, r := range v {
		if r < 0x20 && r != '\t' || r == 0x7f {
			return invalid("%s must not contain control characters", name)
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Headers returns the headers the preset adds to every request. It validates first.
func (d Destination) Headers() (http.Header, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	h := http.Header{}
	switch d.Preset {
	case Langfuse:
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(d.Secrets["public_key"]+":"+d.Secrets["secret_key"])))
	case Datadog:
		h.Set("dd-api-key", d.Secrets["api_key"])
		h.Set("dd-otlp-source", "llmobs")
	case LangSmith:
		h.Set("x-api-key", d.Secrets["api_key"])
		if p := d.Params["project"]; p != "" {
			h.Set("Langsmith-Project", p)
		}
	case Opik:
		h.Set("Authorization", d.Secrets["api_key"])
		h.Set("Comet-Workspace", d.Params["workspace"])
		if p := d.Params["project"]; p != "" {
			h.Set("projectName", p)
		}
	case Generic:
		for _, name := range sortedKeys(d.Secrets) {
			h.Set(name, d.Secrets[name])
		}
	}
	return h, nil
}
