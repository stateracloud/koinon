// SPDX-License-Identifier: Apache-2.0

// Package connectors embeds the connector types shipped with Koinon
// (connectors/<id>/connector.yaml), validates them against
// schema/connector.schema.json and exposes them as typed values (ADR 0007,
// 0008, 0021).
//
// A connector type is data: auth type, hosts, scopes with risk tiers, the
// presets that map tiers to allow, ask or deny, and the setup guide. Code that
// enforces a grant (the gateway's tool allowlist, OpenShell policy) derives it
// from these values instead of hard-coding scopes.
//
// The embed lives here, next to the data, because go:embed cannot reach
// parent directories.
package connectors

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/templates"
)

// Version is the connector type schema version.
const Version = "v1alpha1"

// APIVersion is the apiVersion every connector type document declares.
const APIVersion = project.ConnectorsGroup + "/" + Version

// Kind is the kind every connector type document declares.
const Kind = "ConnectorType"

//go:embed */connector.yaml
var embedded embed.FS

//go:embed schema/connector.schema.json
var schemaJSON []byte

// Tier is a scope's risk tier (ADR 0008).
type Tier string

// Risk tiers.
const (
	TierRead                   Tier = "read"
	TierReversibleWrite        Tier = "reversible-write"
	TierExternalOrIrreversible Tier = "external-or-irreversible"
	TierForbidden              Tier = "forbidden"
)

// Decision is what a preset does with a tier.
type Decision string

// Decisions.
const (
	Allow Decision = "allow"
	Ask   Decision = "ask"
	Deny  Decision = "deny"
)

// AuthType is how Koinon authenticates to the provider (ADR 0006).
type AuthType string

// Auth types.
const (
	AuthFederated     AuthType = "federated"
	AuthAppCredential AuthType = "app-credential" //nolint:gosec // an auth type name, not a credential
	AuthDelegatedUser AuthType = "delegated-user"
	// AuthStaticToken is a long-lived token of a bot account, for a code host that cannot mint
	// credentials per use (ADR 0063).
	AuthStaticToken AuthType = "static-token"
)

// Type is a validated connector type. Treat values returned by this package
// as read-only.
type Type struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

// Metadata identifies a connector type.
type Metadata struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
}

// Spec is the connector type's content.
type Spec struct {
	Auth  Auth   `json:"auth"`
	Hosts []Host `json:"hosts"`
	Site  *Site  `json:"site,omitempty"`
	// Capabilities is the code-host capability descriptor (ADR 0062); nil for a connector that
	// is not a code host.
	Capabilities *Capabilities `json:"capabilities,omitempty"`
	// CIPaths are the path prefixes whose changes run CI.
	CIPaths []string `json:"ciPaths,omitempty"`
	// CIPathsFoldCase makes the workflow guard match CIPaths without regard to case, for a
	// provider whose file system ignores it. Default: exact.
	CIPathsFoldCase bool                        `json:"ciPathsFoldCase,omitempty"`
	Targets         Targets                     `json:"targets"`
	Scopes          []Scope                     `json:"scopes"`
	Presets         map[templates.Preset]Preset `json:"presets"`
	SetupGuide      SetupGuide                  `json:"setupGuide"`
}

// Auth describes the credential.
type Auth struct {
	Type                 AuthType          `json:"type"`
	Permissions          map[string]string `json:"permissions,omitempty"`
	ForbiddenPermissions []string          `json:"forbiddenPermissions,omitempty"`
	TokenLifetime        string            `json:"tokenLifetime,omitempty"`
	// TokenWarnAfter is the age of a static token after which Koinon warns (ADR 0063).
	TokenWarnAfter string `json:"tokenWarnAfter,omitempty"`
}

// Host is one host the connector talks to.
type Host struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Site is the default location of a code host (ADR 0062).
type Site struct {
	BaseURL string `json:"baseURL"`
	APIURL  string `json:"apiURL"`
	// APIPath is where the REST API is served on a connection's own host.
	APIPath string `json:"apiPath,omitempty"`
	// SelfHosted means there is no default host: a connection always names its base URL, which
	// may have a sub-path, and BaseURL and APIURL are placeholders.
	SelfHosted bool `json:"selfHosted,omitempty"`
	// MCPHosts are the hosts of MCP servers known to act on the default host besides its web and API
	// hosts, for example api.githubcopilot.com for github.com (see Type.ServesMCP).
	MCPHosts []string `json:"mcpHosts,omitempty"`
}

// Capabilities is a code host's capability descriptor (ADR 0062, 0063).
type Capabilities struct {
	DisplayName         string `json:"displayName"`
	CredentialClass     string `json:"credentialClass"`
	NarrowsRepositories bool   `json:"narrowsRepositories,omitempty"`
	NarrowsPermissions  bool   `json:"narrowsPermissions,omitempty"`
	MaxRepositories     int    `json:"maxRepositories,omitempty"`
	CanDenyCI           bool   `json:"canDenyCI,omitempty"`
	ExactModes          bool   `json:"exactModes,omitempty"`
	// Writes is how commits reach the host: "api" (the default) or "push" (git push from
	// koinon-api, ADR 0062).
	Writes string `json:"writes,omitempty"`
}

// Targets describes what grants are narrowed to.
type Targets struct {
	Kind        string `json:"kind"`
	Pattern     string `json:"pattern"`
	Description string `json:"description"`
}

// Scope is one grantable capability with its risk tier.
type Scope struct {
	ID          string            `json:"id"`
	Tier        Tier              `json:"tier"`
	Description string            `json:"description"`
	Permissions map[string]string `json:"permissions"`
	Network     []NetworkRule     `json:"network,omitempty"`
	Tools       []string          `json:"tools,omitempty"`
}

// NetworkRule is a direct call the sandbox may make (plan item C2).
type NetworkRule struct {
	Host    string   `json:"host"`
	Methods []string `json:"methods"`
	Paths   []string `json:"paths"`
	// Query maps a query parameter to the glob each of its values must match.
	Query map[string]string `json:"query,omitempty"`
	// Deny makes the rule a deny rule, which wins over every allow rule.
	Deny bool `json:"deny,omitempty"`
}

// Preset maps each tier to a decision.
type Preset map[Tier]Decision

// SetupGuide is the machine-readable setup guide (ADR 0021).
type SetupGuide struct {
	Fields []Field `json:"fields"`
	Steps  []Step  `json:"steps"`
}

// Field is one input of the setup guide.
type Field struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Help     string `json:"help,omitempty"`
	Type     string `json:"type"`
	Secret   bool   `json:"secret,omitempty"`
	Required *bool  `json:"required,omitempty"`
	Pattern  string `json:"pattern,omitempty"`
}

// IsRequired reports whether the field must be filled (the default).
func (f Field) IsRequired() bool { return f.Required == nil || *f.Required }

// Step is one ordered step of the setup guide.
type Step struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Links  []Link   `json:"links,omitempty"`
	Values []Value  `json:"values,omitempty"`
	Fields []string `json:"fields,omitempty"`
	Check  string   `json:"check,omitempty"`
}

// Link points into the provider's console or docs.
type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Value is a setup-specific value to copy.
type Value struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Scope returns the scope with the given ID.
func (t *Type) Scope(id string) (Scope, bool) {
	i := slices.IndexFunc(t.Spec.Scopes, func(s Scope) bool { return s.ID == id })
	if i < 0 {
		return Scope{}, false
	}
	return t.Spec.Scopes[i], true
}

// Decide returns what preset does with scope. Unknown presets and scopes are
// denied.
func (t *Type) Decide(preset templates.Preset, scope string) Decision {
	s, ok := t.Scope(scope)
	if !ok {
		return Deny
	}
	p, ok := t.Spec.Presets[preset]
	if !ok {
		return Deny
	}
	d, ok := p[s.Tier]
	if !ok {
		return Deny
	}
	return d
}

// ToolDecision is a tool a grant exposes and the decision for calling it.
type ToolDecision struct {
	Tool     string
	Decision Decision
	// Scopes are the granted, non-denied scopes that expose the tool.
	Scopes []string
}

// Tools returns the MCP tools a grant of scopes under preset exposes, sorted
// by name: the union over the scopes the preset does not deny. A tool exposed
// by several scopes gets the most permissive of their decisions (allow over
// ask), since any one of those scopes would let the agent call it; tools
// whose effect depends on the scope (create_pull_request) are each exposed by
// exactly one scope. Scopes unknown to the type are ignored.
func (t *Type) Tools(preset templates.Preset, scopes []string) []ToolDecision {
	byTool := map[string]*ToolDecision{}
	for _, id := range scopes {
		d := t.Decide(preset, id)
		if d == Deny {
			continue
		}
		s, _ := t.Scope(id)
		for _, tool := range s.Tools {
			td, ok := byTool[tool]
			if !ok {
				td = &ToolDecision{Tool: tool, Decision: d}
				byTool[tool] = td
			}
			if d == Allow {
				td.Decision = Allow
			}
			td.Scopes = append(td.Scopes, id)
		}
	}
	out := make([]ToolDecision, 0, len(byTool))
	for _, td := range byTool {
		out = append(out, *td)
	}
	slices.SortFunc(out, func(a, b ToolDecision) int {
		switch {
		case a.Tool < b.Tool:
			return -1
		case a.Tool > b.Tool:
			return 1
		}
		return 0
	})
	return out
}

// Permissions returns the union of the provider permissions of the given
// scopes (write wins over read), the narrowest credential that serves them.
func (t *Type) Permissions(scopes ...string) map[string]string {
	out := map[string]string{}
	for _, id := range scopes {
		s, ok := t.Scope(id)
		if !ok {
			continue
		}
		for p, level := range s.Permissions {
			if out[p] != "write" {
				out[p] = level
			}
		}
	}
	return out
}

// ValidTarget reports whether target matches the type's target pattern.
func (t *Type) ValidTarget(target string) bool {
	re, err := regexp.Compile(t.Spec.Targets.Pattern)
	return err == nil && re.MatchString(target)
}

// Schema returns the JSON Schema for connector type documents.
func Schema() []byte { return bytes.Clone(schemaJSON) }

var compiledSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		return nil, fmt.Errorf("decode connector schema: %w", err)
	}
	m, _ := doc.(map[string]any)
	id, _ := m["$id"].(string)
	if id == "" {
		return nil, errors.New("connector schema has no $id")
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(id, doc); err != nil {
		return nil, fmt.Errorf("add connector schema: %w", err)
	}
	return c.Compile(id)
})

// Parse decodes one YAML connector type document and validates it against
// the schema and the rules the schema cannot express.
func Parse(data []byte) (*Type, error) {
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
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("convert YAML to JSON: %w", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	sch, err := compiledSchema()
	if err != nil {
		return nil, err
	}
	if err := sch.Validate(inst); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	var t Type
	jd := json.NewDecoder(bytes.NewReader(raw))
	jd.DisallowUnknownFields()
	if err := jd.Decode(&t); err != nil {
		return nil, fmt.Errorf("decode connector type: %w", err)
	}
	if err := t.check(); err != nil {
		return nil, err
	}
	return &t, nil
}

// check enforces the rules JSON Schema cannot express.
func (t *Type) check() error {
	if t.APIVersion != APIVersion || t.Kind != Kind {
		return fmt.Errorf("apiVersion/kind must be %s/%s", APIVersion, Kind)
	}
	if _, err := regexp.Compile(t.Spec.Targets.Pattern); err != nil {
		return fmt.Errorf("targets.pattern: %w", err)
	}
	hosts := map[string]bool{}
	for _, h := range t.Spec.Hosts {
		hosts[h.Host] = true
	}
	if st := t.Spec.Site; st != nil {
		for _, raw := range []string{st.BaseURL, st.APIURL} {
			hp, err := parseHostPort(raw)
			if err != nil {
				return fmt.Errorf("site: %w", err)
			}
			if !slices.ContainsFunc(t.Spec.Hosts, func(h Host) bool { return h.Host == hp.Host && h.Port == hp.Port }) {
				return fmt.Errorf("site: %s is not in spec.hosts", raw)
			}
		}
	}
	for _, p := range t.Spec.CIPaths {
		if strings.Contains(p, "..") || strings.Contains(p, "//") {
			return fmt.Errorf("ciPaths: %q is not a clean relative path", p)
		}
	}
	if t.Spec.Auth.TokenLifetime != "" {
		if _, err := time.ParseDuration(t.Spec.Auth.TokenLifetime); err != nil {
			return fmt.Errorf("auth.tokenLifetime: %w", err)
		}
	}
	if t.Spec.Auth.TokenWarnAfter != "" {
		if _, err := time.ParseDuration(t.Spec.Auth.TokenWarnAfter); err != nil {
			return fmt.Errorf("auth.tokenWarnAfter: %w", err)
		}
	}
	if st := t.Spec.Site; st != nil && st.SelfHosted && st.APIPath == "" {
		return errors.New("site: a selfHosted connector needs an apiPath")
	}
	if err := t.checkMCPHosts(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i, s := range t.Spec.Scopes {
		if seen[s.ID] {
			return fmt.Errorf("scopes[%d]: duplicate scope %q", i, s.ID)
		}
		seen[s.ID] = true
		for p := range s.Permissions {
			if slices.Contains(t.Spec.Auth.ForbiddenPermissions, p) {
				return fmt.Errorf("scope %s: permission %q is forbidden by auth.forbiddenPermissions", s.ID, p)
			}
			if _, ok := t.Spec.Auth.Permissions[p]; !ok {
				return fmt.Errorf("scope %s: permission %q is not among auth.permissions", s.ID, p)
			}
		}
		if len(s.Network) > 0 && s.Tier != TierRead {
			return fmt.Errorf("scope %s: only read-tier scopes may have network rules; writes go through tools (ADR 0042)", s.ID)
		}
		for _, n := range s.Network {
			if !hosts[n.Host] {
				return fmt.Errorf("scope %s: network host %q is not in spec.hosts", s.ID, n.Host)
			}
			if n.Deny && slices.ContainsFunc(n.Paths, hasTargetPlaceholder) {
				return fmt.Errorf("scope %s: deny rules cannot use {owner} or {repo}", s.ID)
			}
		}
		if s.Tier != TierRead && len(s.Tools) == 0 {
			return fmt.Errorf("scope %s: a %s scope needs at least one tool (ADR 0008)", s.ID, s.Tier)
		}
	}
	fields := map[string]Field{}
	for _, f := range t.Spec.SetupGuide.Fields {
		if _, dup := fields[f.ID]; dup {
			return fmt.Errorf("setupGuide: duplicate field %q", f.ID)
		}
		if f.Pattern != "" {
			if _, err := regexp.Compile(f.Pattern); err != nil {
				return fmt.Errorf("setupGuide field %s: pattern: %w", f.ID, err)
			}
		}
		fields[f.ID] = f
	}
	used := map[string]bool{}
	steps := map[string]bool{}
	for _, s := range t.Spec.SetupGuide.Steps {
		if steps[s.ID] {
			return fmt.Errorf("setupGuide: duplicate step %q", s.ID)
		}
		steps[s.ID] = true
		for _, f := range s.Fields {
			if _, ok := fields[f]; !ok {
				return fmt.Errorf("setupGuide step %s: unknown field %q", s.ID, f)
			}
			if used[f] {
				return fmt.Errorf("setupGuide step %s: field %q is already entered in an earlier step", s.ID, f)
			}
			used[f] = true
		}
	}
	for id := range fields {
		if !used[id] {
			return fmt.Errorf("setupGuide: field %q is not entered in any step", id)
		}
	}
	return nil
}

// Load parses every <id>/connector.yaml under fsys. The directory name must
// equal the type's ID. The result is sorted by ID.
func Load(fsys fs.FS) ([]*Type, error) {
	names, err := fs.Glob(fsys, "*/connector.yaml")
	if err != nil {
		return nil, err
	}
	out := make([]*Type, 0, len(names))
	for _, name := range names {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		t, err := Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if dir := path.Dir(name); dir != t.Metadata.ID {
			return nil, fmt.Errorf("%s: directory must be named %s", name, t.Metadata.ID)
		}
		out = append(out, t)
	}
	return out, nil
}

var all = sync.OnceValues(func() ([]*Type, error) { return Load(embedded) })

// All returns the connector types embedded in the binary, sorted by ID.
func All() ([]*Type, error) { return all() }

// Get returns the embedded connector type with the given ID.
func Get(id string) (*Type, bool) {
	ts, err := all()
	if err != nil {
		return nil, false
	}
	i := slices.IndexFunc(ts, func(t *Type) bool { return t.Metadata.ID == id })
	if i < 0 {
		return nil, false
	}
	return ts[i], true
}

// GitHubID is the ID of the GitHub connector type (ADR 0036).
const GitHubID = "github"
