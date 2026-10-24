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

	"github.com/stateracloud/koinon/internal/catalog"
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
	KeyCredentialHeader = "credentialHeader"
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
)

// Data keys of a connection Secret.
const (
	KeyConnector      = "connector"
	KeyAppID          = "appId"
	KeyInstallationID = "installationId"
	KeyPrivateKey     = "privateKey"
)

// maxValue bounds one Secret value; a Kubernetes Secret is at most 1 MiB in all.
const maxValue = 64 << 10

type modelEntry struct {
	Name  string `yaml:"name"`
	Guard bool   `yaml:"guard"`
	Rates *struct {
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
	}
	if raw := sec.Data[KeyModels]; len(bytes.TrimSpace(raw)) > 0 {
		if len(raw) > maxValue {
			errs = append(errs, fmt.Errorf("%s is longer than %d bytes", KeyModels, maxValue))
		} else {
			var entries []modelEntry
			dec := yaml.NewDecoder(bytes.NewReader(raw))
			dec.KnownFields(true)
			if err := dec.Decode(&entries); err != nil {
				errs = append(errs, fmt.Errorf("%s is not a YAML list of {name, guard, rates}: %w", KeyModels, yamlErr(err)))
			}
			for _, e := range entries {
				if e.Rates != nil {
					d.Warnings = append(d.Warnings, fmt.Sprintf("model %q: rates are ignored; per-model rates were replaced by org prices (add one on the Prices page)", e.Name))
				}
				d.Models = append(d.Models, catalog.DeclaredModel{Name: e.Name, Guard: e.Guard})
			}
		}
	}
	if len(errs) == 0 {
		// Validate the models now, so an invalid Secret is reported before anything is applied.
		for _, m := range d.Models {
			if err := catalog.ValidateModel(m.Name, m.Guard, d.Type); err != nil {
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
}

// ParseConnection reads a connection Secret.
func ParseConnection(sec *corev1.Secret, version string) (DeclaredConnection, error) {
	get := func(k string) string { return strings.TrimSpace(string(sec.Data[k])) }
	d := DeclaredConnection{SecretName: sec.Name, Version: version, DisplayName: get(KeyName), Connector: get(KeyConnector)}
	var errs []error
	if d.DisplayName == "" || len(d.DisplayName) > 200 {
		errs = append(errs, fmt.Errorf("%s is required and at most 200 characters", KeyName))
	}
	if d.Connector != "github" {
		errs = append(errs, fmt.Errorf("%s must be github (the only connector with a declarative form)", KeyConnector))
	}
	for k, dst := range map[string]*int64{KeyAppID: &d.AppID, KeyInstallationID: &d.InstallationID} {
		n, err := strconv.ParseInt(get(k), 10, 64)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("%s must be a positive number", k))
		}
		*dst = n
	}
	d.PrivateKeyPEM = sec.Data[KeyPrivateKey]
	key, err := github.ParsePrivateKey(d.PrivateKeyPEM)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: %s", KeyPrivateKey, strings.TrimPrefix(err.Error(), "github: ")))
	} else {
		d.Fingerprint = key.Fingerprint()
	}
	return d, errors.Join(errs...)
}
