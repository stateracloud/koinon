// SPDX-License-Identifier: Apache-2.0

package declarative

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/gitea"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/project"
)

// Labels on declarative Secrets, in Argo CD's style
// (https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/).
var (
	// LabelSecretType selects the Secrets Koinon imports, and says what they declare.
	LabelSecretType = project.Domain + "/secret-type"
	// LabelOrg names the org slug the Secret declares for.
	LabelOrg = project.Domain + "/org"
)

// Secret types.
const (
	TypeModelProvider = "model-provider"
	TypeConnection    = "connection"
	TypeMCPServer     = "mcp-server"
)

// Data keys of an mcp-server Secret. credentialScheme "Bearer" sends "Bearer <credential>"; without
// it the credential is the whole header value. credentialHeader defaults to Authorization.
const (
	KeyTransport        = "transport"
	KeyCredentialHeader = "credentialHeader" //nolint:gosec // a data key name
	KeyCredentialScheme = "credentialScheme"
	KeyCredential       = "credential"
)

// Data keys of a model-provider Secret.
const (
	KeyName     = "name"
	KeyURL      = "url"
	KeyAPIKey   = "apiKey"
	KeyProvider = "provider"
	KeyModels   = "models"
	// KeyAnthropicWorkspaceID is optional, for provider anthropic only (ADR 0069).
	KeyAnthropicWorkspaceID = "anthropicWorkspaceId"
	// KeyCostAPIKey and KeyCostScope are optional, for anthropic and openai providers at their own
	// API URL: the admin key the daily cost reconciliation reads, and the workspace, project or API
	// key whose cost is the provider's (ADR 0056, amendment 2026-11-06).
	KeyCostAPIKey = "costApiKey" //nolint:gosec // a data key name
	KeyCostScope  = "costScope"
)

// Data keys of a connection Secret.
const (
	KeyConnector      = "connector"
	KeyAppID          = "appId"
	KeyInstallationID = "installationId"
	KeyPrivateKey     = "privateKey"
	KeyBaseURL        = "baseUrl"    // GitHub: optional, a GitHub Enterprise Server host. Gitea: required, https://host[:port][/path]
	KeyCABundle       = "caBundle"   // optional: the public CA bundle of that host
	KeyReadToken      = "readToken"  // Gitea: the bot's read token
	KeyWriteToken     = "writeToken" // Gitea: the bot's write token
	KeyCIDirs         = "ciDirs"     // optional: directories that run CI on the host, one per line or comma-separated
)

// maxValue bounds one Secret value; a Kubernetes Secret is at most 1 MiB in all.
const maxValue = 64 << 10

type modelEntry struct {
	Name  string `yaml:"name"`
	Guard bool   `yaml:"guard"`
	// Format overrides the model's wire format; omitted means the default (ADR 0069).
	Format string `yaml:"format"`
	Rates  *struct {
		Input     string `yaml:"input"`
		Output    string `yaml:"output"`
		CacheRead string `yaml:"cacheRead"`
	} `yaml:"rates"`
}

// ParseProvider reads a model-provider Secret. The error is safe to show: it never quotes a value.
func ParseProvider(sec *corev1.Secret, version string) (catalog.DeclaredProvider, error) {
	get := func(k string) string { return strings.TrimSpace(string(sec.Data[k])) }
	d := catalog.DeclaredProvider{
		SecretName: sec.Name, Version: version,
		Name: get(KeyName), Type: get(KeyProvider), URL: get(KeyURL), APIKey: get(KeyAPIKey),
		AnthropicWorkspaceID: get(KeyAnthropicWorkspaceID),
		CostAPIKey:           get(KeyCostAPIKey), CostScope: get(KeyCostScope),
	}
	var errs []error
	for _, k := range []string{KeyName, KeyURL, KeyProvider} {
		if get(k) == "" {
			errs = append(errs, fmt.Errorf("%s is required", k))
		}
	}
	if len(errs) == 0 {
		if err := catalog.ValidateProvider(d.Name, d.Type, d.URL); err != nil {
			errs = append(errs, err)
		}
		if err := catalog.ValidateWorkspaceID(d.Type, d.AnthropicWorkspaceID); err != nil {
			errs = append(errs, err)
		}
		if err := catalog.ValidateCostKey(d.Type, d.URL, d.CostAPIKey != "", d.CostScope); err != nil {
			errs = append(errs, err)
		}
	}
	if raw := sec.Data[KeyModels]; len(bytes.TrimSpace(raw)) > 0 {
		if len(raw) > maxValue {
			errs = append(errs, fmt.Errorf("%s is longer than %d bytes", KeyModels, maxValue))
		} else {
			var entries []modelEntry
			dec := yaml.NewDecoder(bytes.NewReader(raw))
			dec.KnownFields(true)
			if err := dec.Decode(&entries); err != nil {
				errs = append(errs, fmt.Errorf("%s is not a YAML list of {name, guard, format}: %w", KeyModels, yamlErr(err)))
			}
			for _, e := range entries {
				if e.Rates != nil {
					d.Warnings = append(d.Warnings, fmt.Sprintf("model %q: rates are ignored; per-model rates were replaced by org prices (add one on the Prices page)", e.Name))
				}
				d.Models = append(d.Models, catalog.DeclaredModel{Name: e.Name, Guard: e.Guard, Format: e.Format})
			}
		}
	}
	if len(errs) == 0 {
		// Validate the models now, so an invalid Secret is reported before anything is applied.
		for _, m := range d.Models {
			if err := catalog.ValidateModel(m.Name, m.Guard, d.Type); err != nil {
				errs = append(errs, fmt.Errorf("model %q: %w", m.Name, err))
			}
			if err := catalog.ValidateModelFormat(d.Type, m.Format); err != nil {
				errs = append(errs, fmt.Errorf("model %q: %w", m.Name, err))
			}
		}
	}
	return d, errors.Join(errs...)
}

// yamlErr drops anything of the document the YAML library might echo.
func yamlErr(err error) error {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		return errors.New("a field has the wrong type or an unknown name")
	}
	return errors.New("invalid YAML")
}

// ParseMCPServer reads an mcp-server Secret. The error is safe to show: it never quotes a value.
func ParseMCPServer(sec *corev1.Secret, version string) (mcpservers.Declared, error) {
	get := func(k string) string { return strings.TrimSpace(string(sec.Data[k])) }
	d := mcpservers.Declared{
		SecretName: sec.Name, Version: version, Name: get(KeyName), URL: get(KeyURL), Transport: get(KeyTransport),
		CredentialHeader: get(KeyCredentialHeader), CredentialValue: get(KeyCredential),
	}
	if d.Transport == "" {
		d.Transport = mcpservers.TransportStreamableHTTP
	}
	if scheme := get(KeyCredentialScheme); scheme != "" {
		d.CredentialPrefix = scheme + " "
	}
	var errs []error
	for _, k := range []string{KeyName, KeyURL} {
		if get(k) == "" {
			errs = append(errs, fmt.Errorf("%s is required", k))
		}
	}
	if d.CredentialValue == "" && (d.CredentialHeader != "" || d.CredentialPrefix != "") {
		errs = append(errs, fmt.Errorf("%s is required when a credential header or scheme is set", KeyCredential))
	}
	if len(errs) == 0 {
		header := d.CredentialHeader
		if header == "" && d.CredentialValue != "" {
			header = "Authorization"
		}
		if err := mcpservers.ValidateName(d.Name); err != nil {
			errs = append(errs, err)
		} else if err := mcpservers.ValidateRemote(d.URL, d.Transport, header, d.CredentialPrefix); err != nil {
			errs = append(errs, err)
		}
	}
	return d, errors.Join(errs...)
}

// DeclaredConnection is a connection declared by a Secret of type connection.
type DeclaredConnection struct {
	SecretName     string
	Version        string
	DisplayName    string
	Connector      string
	AppID          int64
	InstallationID int64
	PrivateKeyPEM  []byte
	Fingerprint    string
	Site           codehost.Site // the connection's own host; the zero value is github.com
	// Gitea (ADR 0063): the bot's two tokens.
	ReadToken, WriteToken []byte
}

// SecretFields are the connection's secret fields, by setup-guide field ID, for the secret store.
func (d DeclaredConnection) SecretFields() map[string][]byte {
	if d.Connector == gitea.HostID {
		return map[string][]byte{gitea.ReadTokenField: d.ReadToken, gitea.WriteTokenField: d.WriteToken}
	}
	return map[string][]byte{connections.GitHubPrivateKeyField: d.PrivateKeyPEM}
}

// ParseConnection reads a connection Secret.
func ParseConnection(sec *corev1.Secret, version string) (DeclaredConnection, error) {
	get := func(k string) string { return strings.TrimSpace(string(sec.Data[k])) }
	d := DeclaredConnection{SecretName: sec.Name, Version: version, DisplayName: get(KeyName), Connector: get(KeyConnector)}
	var errs []error
	if d.DisplayName == "" || len(d.DisplayName) > 200 {
		errs = append(errs, fmt.Errorf("%s is required and at most 200 characters", KeyName))
	}
	switch d.Connector {
	case github.HostID, gitea.HostID:
	default:
		errs = append(errs, fmt.Errorf("%s must be github or gitea (the connectors with a declarative form)", KeyConnector))
	}
	if d.Connector == gitea.HostID {
		errs = append(errs, parseGitea(&d, get)...)
		return d, errors.Join(errs...)
	}
	for k, dst := range map[string]*int64{KeyAppID: &d.AppID, KeyInstallationID: &d.InstallationID} {
		n, err := strconv.ParseInt(get(k), 10, 64)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("%s must be a positive number", k))
		}
		*dst = n
	}
	errs = append(errs, parseSite(&d, get)...)
	d.PrivateKeyPEM = sec.Data[KeyPrivateKey]
	key, err := github.ParsePrivateKey(d.PrivateKeyPEM)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: %s", KeyPrivateKey, strings.TrimPrefix(err.Error(), "github: ")))
	} else {
		d.Fingerprint = key.Fingerprint()
	}
	return d, errors.Join(errs...)
}

// parseSite reads the host keys common to code-host connections: baseUrl, caBundle and ciDirs.
func parseSite(d *DeclaredConnection, get func(string) string) []error {
	var errs []error
	if t, ok := connectors.Get(d.Connector); ok {
		loc, err := t.Locate(get(KeyBaseURL))
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", KeyBaseURL, err))
		case !loc.Default:
			d.Site.BaseURL = loc.BaseURL()
		}
		if bundle := get(KeyCABundle); bundle != "" {
			if _, err := codehost.CertPool(bundle); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", KeyCABundle, err))
			} else {
				d.Site.CABundle = bundle
			}
		}
	}
	if v := get(KeyCIDirs); v != "" {
		dirs, err := codehost.CleanCIDirs(strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", KeyCIDirs, err))
		}
		d.Site.CIDirs = dirs
	}
	return errs
}

// parseGitea reads the keys of a Gitea connection Secret. Errors never quote a token.
func parseGitea(d *DeclaredConnection, get func(string) string) []error {
	errs := parseSite(d, get)
	d.ReadToken, d.WriteToken = []byte(get(KeyReadToken)), []byte(get(KeyWriteToken))
	if err := (&gitea.Host{}).ValidateSecrets(map[string][]byte{gitea.ReadTokenField: d.ReadToken, gitea.WriteTokenField: d.WriteToken}); err != nil {
		errs = append(errs, fmt.Errorf("%s, %s: %s", KeyReadToken, KeyWriteToken, strings.TrimPrefix(err.Error(), "gitea: ")))
	} else {
		d.Fingerprint = gitea.Fingerprint(d.ReadToken) + "." + gitea.Fingerprint(d.WriteToken)
	}
	return errs
}
