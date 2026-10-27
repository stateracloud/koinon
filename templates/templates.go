// SPDX-License-Identifier: Apache-2.0

// Package templates embeds the agent templates shipped with Koinon
// (templates/*.yaml), validates them against schema/template.schema.json and
// exposes them as typed values with a stable content hash (ADR 0020, 0027).
//
// The embed lives here, next to the data, because go:embed cannot reach
// parent directories.
package templates

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/flavor"
	"github.com/stateracloud/koinon/internal/project"
)

// Version is the template schema version.
const Version = "v1alpha1"

// APIVersion is the apiVersion every template document declares.
const APIVersion = project.TemplatesGroup + "/" + Version

// Kind is the kind every template document declares.
const Kind = "Template"

// MaxRunDuration bounds limits.run.maxDuration.
const MaxRunDuration = 24 * time.Hour

//go:embed *.yaml
var embedded embed.FS

//go:embed schema/template.schema.json
var schemaJSON []byte

// Flavor is a harness an agent runs on (ADR 0035).
type Flavor string

// Flavors shipped in v0.1.
const (
	FlavorOpenCode Flavor = "opencode"
)

// AllFlavors lists every flavor. The schema's $defs.flavor enum and the
// agent_flavor type in the migrations hold the same list; a test keeps the
// three in step.
var AllFlavors = allFlavors()

func allFlavors() []Flavor {
	var out []Flavor
	for _, n := range flavor.Names() {
		out = append(out, Flavor(n))
	}
	return out
}

// Preset maps a grant's risk tiers to allow, ask or deny (ADR 0008, 0049).
type Preset string

// Approval presets.
const (
	PresetReadOnly Preset = "read-only"
	PresetStandard Preset = "standard"
	PresetTrusted  Preset = "trusted"
)

// Template is a validated agent template. Treat values returned by this
// package as read-only.
type Template struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`

	// Hash is "sha256:<hex>" over the document's canonical JSON form, so
	// comments and formatting do not change it. Sync uses it to version
	// templates in the database.
	Hash string `json:"-"`
}

// Metadata identifies a template.
type Metadata struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
}

// Spec is what an agent draft starts from.
type Spec struct {
	Flavors Flavors `json:"flavors"`
	Harness Harness `json:"harness,omitzero"`
	// Ephemeral makes agents created from the template ephemeral: each task gets a fresh sandbox
	// without a persistent home, deleted when the task ends (ADR 0065 decision 13c). Off by default.
	Ephemeral    bool    `json:"ephemeral,omitempty"`
	Model        Model   `json:"model"`
	Instructions string  `json:"instructions"`
	Grants       []Grant `json:"grants"`
	Limits       Limits  `json:"limits"`
}

// Harness holds harness settings beyond the flavor choice. The zero value turns every
// setting off.
type Harness struct {
	// PluginInstalls lets an OpenCode agent install plugins: its sandbox may read the npm
	// registry (GET and HEAD, no credentials) from the OpenCode binary, and OpenCode loads
	// external plugins. Off by default; OpenCode only.
	PluginInstalls bool `json:"pluginInstalls,omitempty"`
}

// Flavors lists the allowed harnesses and the default choice.
type Flavors struct {
	Allowed []Flavor `json:"allowed"`
	Default Flavor   `json:"default"`
}

// Model is a logical model name, resolved against the install's model list
// when an agent is created. "default" is the install's default model.
type Model struct {
	Name string `json:"name"`
}

// Grant is a suggested grant on a connector type (ADR 0007). It is a
// proposal; an admin or owner approves it when activating the agent.
type Grant struct {
	Connector string   `json:"connector"`
	Scopes    []string `json:"scopes"`
	Preset    Preset   `json:"preset"`
	Targets   *Targets `json:"targets,omitempty"`
	Purpose   string   `json:"purpose,omitempty"`
}

// Targets bounds how many targets of a connection (for GitHub:
// repositories) the user picks when creating the agent.
type Targets struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// Limits are the template's run caps and spend limit.
type Limits struct {
	Run      RunLimits `json:"run"`
	SpendUSD USD       `json:"spendUSD"`
}

// RunLimits cap a single run; hitting one cancels the run.
type RunLimits struct {
	MaxDuration Duration `json:"maxDuration"`
	MaxTokens   int64    `json:"maxTokens"`
}

// Duration is a time.Duration written as a Go duration string.
type Duration struct{ time.Duration }

// UnmarshalJSON parses a duration string such as "30m".
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// USD is an amount in US cents, written as a decimal string such as "10.00".
type USD int64

// Cents returns the amount in cents.
func (u USD) Cents() int64 { return int64(u) }

// String formats the amount with two fraction digits.
func (u USD) String() string { return fmt.Sprintf("%d.%02d", u/100, u%100) }

// UnmarshalJSON parses a decimal string with at most two fraction digits.
func (u *USD) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 2 {
		return fmt.Errorf("USD amount %q has more than two fraction digits", s)
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w < 0 {
		return fmt.Errorf("invalid USD amount %q", s)
	}
	var c int64
	if frac != "" {
		c, err = strconv.ParseInt(frac+strings.Repeat("0", 2-len(frac)), 10, 64)
		if err != nil || c < 0 {
			return fmt.Errorf("invalid USD amount %q", s)
		}
	}
	*u = USD(w*100 + c)
	return nil
}

// MarshalJSON writes the amount as a decimal string.
func (u USD) MarshalJSON() ([]byte, error) { return json.Marshal(u.String()) }

// Schema returns the JSON Schema for template documents.
func Schema() []byte { return bytes.Clone(schemaJSON) }

var compiledSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		return nil, fmt.Errorf("decode template schema: %w", err)
	}
	// Register the schema under its own $id so errors name it, not a local path.
	m, _ := doc.(map[string]any)
	id, _ := m["$id"].(string)
	if id == "" {
		return nil, errors.New("template schema has no $id")
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(id, doc); err != nil {
		return nil, fmt.Errorf("add template schema: %w", err)
	}
	return c.Compile(id)
})

// Parse decodes one YAML template document, validates it against the schema
// and the rules the schema cannot express, and computes its hash.
func Parse(data []byte) (*Template, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc any
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty document")
		}
		return nil, fmt.Errorf("decode YAML: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("expected exactly one YAML document")
	}

	canonical, err := canonicalJSON(doc)
	if err != nil {
		return nil, err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(canonical))
	if err != nil {
		return nil, fmt.Errorf("decode canonical JSON: %w", err)
	}
	sch, err := compiledSchema()
	if err != nil {
		return nil, err
	}
	if err := sch.Validate(inst); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}

	var t Template
	jd := json.NewDecoder(bytes.NewReader(canonical))
	jd.DisallowUnknownFields()
	if err := jd.Decode(&t); err != nil {
		return nil, fmt.Errorf("decode template: %w", err)
	}
	if err := t.check(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	t.Hash = "sha256:" + hex.EncodeToString(sum[:])
	return &t, nil
}

// canonicalJSON renders a decoded YAML value as JSON with sorted keys and no
// HTML escaping, the input to both validation and the content hash.
func canonicalJSON(doc any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("convert YAML to JSON: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// check enforces the rules JSON Schema cannot express.
func (t *Template) check() error {
	if t.APIVersion != APIVersion || t.Kind != Kind {
		return fmt.Errorf("apiVersion/kind must be %s/%s", APIVersion, Kind)
	}
	if !slices.Contains(t.Spec.Flavors.Allowed, t.Spec.Flavors.Default) {
		return fmt.Errorf("default flavor %q is not in the allowed flavors", t.Spec.Flavors.Default)
	}
	if t.Spec.Harness.PluginInstalls && !slices.ContainsFunc(t.Spec.Flavors.Allowed, func(f Flavor) bool {
		return flavor.SupportsPluginInstalls(store.AgentFlavor(f))
	}) {
		var names []string
		for _, d := range flavor.All() {
			if d.PluginInstalls {
				names = append(names, string(d.Name))
			}
		}
		return fmt.Errorf("harness.pluginInstalls applies to the %s flavor only, which is not allowed", strings.Join(names, "/"))
	}
	if d := t.Spec.Limits.Run.MaxDuration.Duration; d > MaxRunDuration {
		return fmt.Errorf("limits.run.maxDuration %s exceeds %s", d, MaxRunDuration)
	}
	if t.Spec.Limits.SpendUSD <= 0 {
		return errors.New("limits.spendUSD must be greater than zero")
	}
	seen := map[string]bool{}
	for i, g := range t.Spec.Grants {
		if seen[g.Connector] {
			return fmt.Errorf("grants[%d]: more than one grant for connector %q", i, g.Connector)
		}
		seen[g.Connector] = true
		if g.Targets != nil && g.Targets.Min > g.Targets.Max {
			return fmt.Errorf("grants[%d].targets: min %d is greater than max %d", i, g.Targets.Min, g.Targets.Max)
		}
	}
	return nil
}

// Load parses every *.yaml file at the root of fsys. Each file must be named
// after its template ID, and IDs must be unique. The result is sorted by ID.
func Load(fsys fs.FS) ([]*Template, error) {
	names, err := fs.Glob(fsys, "*.yaml")
	if err != nil {
		return nil, err
	}
	out := make([]*Template, 0, len(names))
	for _, name := range names {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		t, err := Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if want := t.Metadata.ID + ".yaml"; path.Base(name) != want {
			return nil, fmt.Errorf("%s: file name must be %s", name, want)
		}
		out = append(out, t)
	}
	// File names equal IDs and are unique within a directory, so IDs are
	// unique too; Glob already returns them sorted.
	return out, nil
}

var all = sync.OnceValues(func() ([]*Template, error) { return Load(embedded) })

// All returns the templates embedded in the binary, sorted by ID.
func All() ([]*Template, error) { return all() }

// Get returns the embedded template with the given ID.
func Get(id string) (*Template, bool) {
	ts, err := all()
	if err != nil {
		return nil, false
	}
	i := slices.IndexFunc(ts, func(t *Template) bool { return t.Metadata.ID == id })
	if i < 0 {
		return nil, false
	}
	return ts[i], true
}
