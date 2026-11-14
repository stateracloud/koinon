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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/stateracloud/koinon/internal/harness"
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

// HarnessName names a harness an agent runs on (ADR 0112).
type HarnessName string

// The built-in harnesses.
const (
	HarnessOpenCode HarnessName = "opencode"
)

// AllHarnesses lists every built-in harness. The schema's $defs.harness enum holds the same list; a
// test keeps the two in step.
var AllHarnesses = allHarnesses()

func allHarnesses() []HarnessName {
	var out []HarnessName
	for _, n := range harness.Names() {
		out = append(out, HarnessName(n))
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
	Harness Harness `json:"harness"`
	// Ephemeral makes agents created from the template ephemeral: each task gets a fresh sandbox
	// without a persistent home, deleted when the task ends (ADR 0065 decision 13c). Off by default.
	Ephemeral    bool   `json:"ephemeral,omitempty"`
	Model        Model  `json:"model"`
	Instructions string `json:"instructions"`
	// Tools names the tools the instructions expect the agent to call, by tool name (the names the
	// MCP guard classifies, ADR 0091). Optional. Parse refuses a tool the instructions name in
	// backticks that this list lacks; CheckCatalog refuses a name no connector or declaration
	// knows; the templates legs test reads it, so a tool the instructions only describe in prose is
	// still classified.
	Tools  []string `json:"tools,omitempty"`
	Grants []Grant  `json:"grants"`
	Limits Limits   `json:"limits"`
	// Triggers start work on the agent: a schedule or a direct message in v0.1 (ADR 0066).
	Triggers []Trigger `json:"triggers,omitempty"`
	// Questions sets when a question an agent asked with ask_user gets its reminder and when it
	// expires (ADR 0065 decision 10). Unset values take the defaults: 24h and 7d.
	Questions QuestionTimers `json:"questions,omitzero"`
	// Resources are the CPU and memory requests and limits of the agent's sandbox (ADR 0083). Every
	// value is optional: an unset one takes the platform default, and the platform bounds clamp
	// all of them.
	Resources Resources `json:"resources,omitzero"`
}

// Resources are Kubernetes resource quantities for the agent container of a sandbox (ADR 0083).
type Resources struct {
	CPU    Quantities `json:"cpu,omitzero"`
	Memory Quantities `json:"memory,omitzero"`
}

// Quantities are the request and the limit of one resource, as Kubernetes quantity strings such
// as "250m" or "1Gi". An empty string is unset.
type Quantities struct {
	Request string `json:"request,omitempty"`
	Limit   string `json:"limit,omitempty"`
}

// Validate checks that both values are quantities and that the request does not exceed the limit.
// name is the resource, for the messages.
func (q Quantities) Validate(name string) error {
	var req, lim resource.Quantity
	for _, v := range []struct {
		field, val string
		dst        *resource.Quantity
	}{{"request", q.Request, &req}, {"limit", q.Limit, &lim}} {
		if v.val == "" {
			continue
		}
		parsed, err := resource.ParseQuantity(v.val)
		if err != nil {
			return fmt.Errorf("%s.%s %q: %w", name, v.field, v.val, err)
		}
		if parsed.Sign() <= 0 {
			return fmt.Errorf("%s.%s %q must be greater than zero", name, v.field, v.val)
		}
		*v.dst = parsed
	}
	if q.Request != "" && q.Limit != "" && req.Cmp(lim) > 0 {
		return fmt.Errorf("%s.request %s is greater than %s.limit %s", name, q.Request, name, q.Limit)
	}
	return nil
}

// Validate checks both resources.
func (r Resources) Validate() error {
	if err := r.CPU.Validate("cpu"); err != nil {
		return err
	}
	return r.Memory.Validate("memory")
}

// QuestionTimers are the reminder and expiry of the questions an agent asks (ADR 0065 decision
// 10), as Go durations from the moment the question is asked. A zero value takes the default.
type QuestionTimers struct {
	ReminderAfter Duration `json:"reminderAfter,omitzero"`
	ExpireAfter   Duration `json:"expireAfter,omitzero"`
}

// Bounds of the question timers.
const (
	MinQuestionTimer = time.Minute
	MaxQuestionTimer = 30 * 24 * time.Hour
)

// Validate checks the timers: each within bounds, and the reminder before the expiry.
func (q QuestionTimers) Validate() error {
	for name, d := range map[string]time.Duration{"reminderAfter": q.ReminderAfter.Duration, "expireAfter": q.ExpireAfter.Duration} {
		if d != 0 && (d < MinQuestionTimer || d > MaxQuestionTimer) {
			return fmt.Errorf("questions.%s %s must be between %s and %s", name, d, MinQuestionTimer, MaxQuestionTimer)
		}
	}
	if r, e := q.ReminderAfter.Duration, q.ExpireAfter.Duration; r != 0 && e != 0 && r >= e {
		return fmt.Errorf("questions.reminderAfter %s must be shorter than questions.expireAfter %s", r, e)
	}
	return nil
}

// Harness lists the harnesses an agent may run on, the default choice, and the harness settings.
type Harness struct {
	Allowed []HarnessName `json:"allowed"`
	Default HarnessName   `json:"default"`
	HarnessSettings
}

// HarnessSettings are the harness settings of a template, which an agent's configuration carries
// (internal/agentconfig). The zero value turns every setting off.
type HarnessSettings struct {
	// PluginInstalls lets an OpenCode agent install plugins: its sandbox may read the npm
	// registry (GET and HEAD, no credentials) from the OpenCode binary, and OpenCode loads
	// external plugins. Off by default; OpenCode only.
	PluginInstalls bool `json:"pluginInstalls,omitempty"`
}

// Model is a logical model name, resolved against the install's model list
// when an agent is created. "default" is the install's default model.
type Model struct {
	Name string `json:"name"`
	// Thinking is the template's starting value for the agent's thinking setting: "on" or "off".
	// Unset means the default, on. An admin changes the agent's own setting later; the model's
	// catalog capability decides whether it has any effect (internal/thinking, ADR 0069 amendment).
	Thinking string `json:"thinking,omitempty"`
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

// RunLimits cap a single run; the first one reached cancels it (ADR 0081).
type RunLimits struct {
	// MaxDuration is the wall-clock limit.
	MaxDuration Duration `json:"maxDuration"`
	// MaxModelRequests counts every model request through the agent's key, the harness's own title,
	// summary and compaction calls included. Optional: zero means unset, and a run gets the platform
	// default (ADR 0081). The schema refuses an explicit zero.
	MaxModelRequests int64 `json:"maxModelRequests,omitempty"`
	// MaxSpendUSD is the run's spend by the ledger's settled cost. Optional like MaxModelRequests.
	MaxSpendUSD USD `json:"maxSpendUSD,omitempty"`
	// MaxContinuations is how many continuation runs a task of this agent gets after a run hits its
	// time limit (issue #99, ADR 0081 amendment 2026-11-12). Optional: nil means the platform
	// default of 3; 0 switches continuation off. An admin's per-agent override applies over it.
	MaxContinuations *int `json:"maxContinuations,omitempty"`
	// MaxTokens is deprecated: tokens are reported, not enforced (ADR 0081). A template may still
	// carry it; nothing reads it.
	//
	// Deprecated: tokens are reported, not enforced.
	MaxTokens int64 `json:"maxTokens,omitempty"`
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

// toolCall matches a tool the instructions tell the agent to call, written in backticks.
var toolCall = regexp.MustCompile("`([a-z]+(?:_[a-z]+)+)`")

// BacktickedTools returns the tool names the text writes in backticks, in order of first use and
// without repeats: the tools the instructions tell the agent to call.
func BacktickedTools(text string) []string {
	var names []string
	for _, m := range toolCall.FindAllStringSubmatch(text, -1) {
		if !slices.Contains(names, m[1]) {
			names = append(names, m[1])
		}
	}
	return names
}

// undeclaredTools are the backticked tool names in instructions that tools does not list.
func undeclaredTools(instructions string, tools []string) []string {
	var missing []string
	for _, n := range BacktickedTools(instructions) {
		if !slices.Contains(tools, n) {
			missing = append(missing, n)
		}
	}
	return missing
}

// check enforces the rules JSON Schema cannot express.
func (t *Template) check() error {
	if t.APIVersion != APIVersion || t.Kind != Kind {
		return fmt.Errorf("apiVersion/kind must be %s/%s", APIVersion, Kind)
	}
	if !slices.Contains(t.Spec.Harness.Allowed, t.Spec.Harness.Default) {
		return fmt.Errorf("default harness %q is not in the allowed harnesses", t.Spec.Harness.Default)
	}
	if t.Spec.Harness.PluginInstalls && !slices.ContainsFunc(t.Spec.Harness.Allowed, func(f HarnessName) bool {
		return harness.SupportsPluginInstalls(string(f))
	}) {
		var names []string
		for _, d := range harness.All() {
			if d.PluginInstalls {
				names = append(names, d.Name)
			}
		}
		return fmt.Errorf("harness.pluginInstalls applies to the %s harness only, which is not allowed", strings.Join(names, "/"))
	}
	if d := t.Spec.Limits.Run.MaxDuration.Duration; d > MaxRunDuration {
		return fmt.Errorf("limits.run.maxDuration %s exceeds %s", d, MaxRunDuration)
	}
	if t.Spec.Limits.SpendUSD <= 0 {
		return errors.New("limits.spendUSD must be greater than zero")
	}
	if missing := undeclaredTools(t.Spec.Instructions, t.Spec.Tools); len(missing) > 0 {
		return fmt.Errorf("spec.tools: the instructions call %s in backticks; add it to spec.tools so the legs check sees it", strings.Join(missing, ", "))
	}
	if err := t.Spec.Questions.Validate(); err != nil {
		return fmt.Errorf("spec.%w", err)
	}
	if err := t.Spec.Resources.Validate(); err != nil {
		return fmt.Errorf("spec.resources.%w", err)
	}
	if err := ValidateTriggers(t.Spec.Triggers); err != nil {
		return fmt.Errorf("spec.%w", err)
	}
	for i, tr := range t.Spec.Triggers {
		// A person and a channel both belong to one org. From v0.2 a template reaches the org's
		// channel through the default, so templates never take a channel, now or later.
		switch {
		case tr.Notify != nil && tr.Notify.Kind == NotifyUser:
			return fmt.Errorf("spec.triggers[%d]: notify: a template cannot name a person; use a role and pick the person on the agent's Edit triggers page", i)
		case tr.Notify != nil && tr.Notify.Kind == NotifyChannel:
			return fmt.Errorf("spec.triggers[%d]: notify: a template cannot name a chat channel; leave notify out to use the org's notify channel", i)
		}
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

// all loads the embedded templates and holds each to the bounds a shipped template sets
// (CheckShipped), so All and Get fail on a missing bound. The catalog check (connector, scope and
// tool names) needs the connectors package, which imports this one: see CheckEmbedded.
var all = sync.OnceValues(func() ([]*Template, error) {
	ts, err := Load(embedded)
	if err != nil {
		return nil, err
	}
	for _, t := range ts {
		if err := t.CheckShipped(); err != nil {
			return nil, fmt.Errorf("%s: %w", t.Metadata.ID, err)
		}
	}
	return ts, nil
})

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
