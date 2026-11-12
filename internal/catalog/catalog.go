// SPDX-License-Identifier: Apache-2.0

// Package catalog manages an org's model providers and models (ADR 0056, plan item PV1).
//
// One service layer serves every caller: the UI's connect-rpc services, the admin API and
// the declarative-Secret importer. Each change is validated, written and audited in one
// transaction, so every route is audited alike. Provider API keys are sealed with the
// secret store (internal/secretstore) and never leave this package in the clear; only the
// reconciler opens them (OpenKey) to deliver them to the gateway.
//
// A provider or model is either entered by a person (source "ui", changed through the UI
// or admin API) or declared by a labelled Kubernetes Secret (source "secret", read-only
// here, see package declarative). A declared entry wins a name collision: the UI entry of
// the same name is disabled with a visible reason until the declared one is gone.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/outward"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// Errors. Messages are safe to show to the caller.
var (
	ErrNotFound = errors.New("not found")
	// ErrReadOnly means the entry is declared by a Secret and can only change there.
	ErrReadOnly = errors.New("declared by a Kubernetes Secret and read-only here")
	ErrConflict = errors.New("an entry with this name already exists")
	ErrNoKeys   = errors.New("no key-encryption keys are configured, so provider keys cannot be stored")
)

func readOnly(secret string) error {
	return fmt.Errorf("%w (managed by the Secret %s; change it there)", ErrReadOnly, secret)
}

// InvalidError is a validation failure.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &InvalidError{Msg: fmt.Sprintf(format, a...)} }

// Keys seals and opens secret values; *secretstore.Keyring is one.
type Keys interface {
	Seal(b secretstore.Binding, plaintext []byte) (secretstore.Envelope, error)
	Open(b secretstore.Binding, e secretstore.Envelope) ([]byte, error)
}

// Table and column of provider keys, as bound into the AAD.
const (
	ProvidersTable = "providers"
	KeyColumn      = "api_key"
)

// KeyBinding is the AAD binding of a provider's API key.
func KeyBinding(org, id pgtype.UUID) secretstore.Binding {
	return secretstore.Binding{OrgID: org.String(), Table: ProvidersTable, RowID: id.String(), Column: KeyColumn}
}

// Sources.
const (
	SourceUI     = "ui"
	SourceSecret = "secret"
)

// Reasons a UI entry is disabled.
const (
	ReasonShadowedProvider = "shadowed by a provider of the same name declared in a Kubernetes Secret"
	ReasonShadowedModel    = "shadowed by a model of the same name declared in a Kubernetes Secret"
	ReasonProviderDisabled = "its provider is disabled"
	// ReasonRemoved marks a declared entry whose Secret was deleted. The row stays (audit, anything
	// that refers to it); restoring the Secret brings it back, and an admin may delete it.
	ReasonRemoved = "its Kubernetes Secret was deleted; restore the Secret or delete this entry"
)

// GuardProviderType is the only provider type with a routing guard: OpenRouter's
// provider-routing preferences (ADR 0048).
const GuardProviderType = "openrouter"

var (
	namePattern  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)
	typePattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	modelPattern = regexp.MustCompile(`^[A-Za-z0-9_.:/@-]{1,256}$`)
)

// WorkspaceProviderType is the only provider type with an Anthropic workspace ID (ADR 0069,
// decision 5).
const WorkspaceProviderType = modelformat.ProviderAnthropic

// Rates are the per-model rates of PV1. They were folded into org price entries (ADR 0056,
// amendment 2026-10-24, W4); the type remains only so a request or Secret that still sets them
// can be refused (API) or ignored with a warning (declarative Secrets).
type Rates struct {
	Input     string `json:"input"`
	Output    string `json:"output"`
	CacheRead string `json:"cacheRead,omitempty"`
}

// ErrRatesRemoved is the answer to a request that sets rates on a model.
const ratesRemoved = "rates on a model were replaced by prices: add one on the Prices page, with PriceService.SetPrice or POST /admin/v1/prices"

// Provider is a provider as callers see it: never its key.
type Provider struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Type            string `json:"type"`
	URL             string `json:"url"`
	HasKey          bool   `json:"hasKey"`
	Source          string `json:"source"`
	ManagedBySecret string `json:"managedBySecret,omitempty"`
	DisabledReason  string `json:"disabledReason,omitempty"`
	// Removed is set when the declaring Secret was deleted.
	Removed bool `json:"removed,omitempty"`
	// CredentialLostAt is set when the API key was lost with the KEK that encrypted it (keys forget,
	// ADR 0085 decision 13): the provider is not served until the key is entered again.
	CredentialLostAt *time.Time `json:"credentialLostAt,omitempty"`
	// AdminDisabled is set while an admin holds the target off (the outward switch, ADR 0110 decision 5).
	AdminDisabled *outward.Disabled `json:"adminDisabled,omitempty"`
	// AnthropicWorkspaceID is the workspace the gateway names for the provider's models (type
	// anthropic only). An identifier, not a credential.
	AnthropicWorkspaceID string `json:"anthropicWorkspaceId,omitempty"`
	// HasCostKey is whether a cost API key is stored; CostScope is the workspace, project or API key
	// whose cost the daily reconciliation compares with the ledger (ADR 0056, amendment 2026-11-06).
	HasCostKey bool   `json:"hasCostKey"`
	CostScope  string `json:"costScope,omitempty"`
	// CostKeyLostAt is set when the cost API key was lost with its KEK; it must be entered again.
	CostKeyLostAt *time.Time `json:"costKeyLostAt,omitempty"`
	// Formats are the model formats the provider's type offers, preferred first (ADR 0069).
	Formats []string `json:"formats"`
	// Serving is whether the gateway serves the provider (issue #17). A provider just written is saved.
	Serving   Serving   `json:"serving"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Model is a model as callers see it.
type Model struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerId"`
	Name       string `json:"name"`
	Guard      bool   `json:"guard"`
	// Format is the model's wire format (ADR 0069); FormatOverridden is set when an admin chose it.
	Format           string    `json:"format"`
	FormatOverridden bool      `json:"formatOverridden"`
	Source           string    `json:"source"`
	ManagedBySecret  string    `json:"managedBySecret,omitempty"`
	DisabledReason   string    `json:"disabledReason,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

// Service is the catalog service layer.
type Service struct {
	pool   *pgxpool.Pool
	keys   Keys
	log    *slog.Logger
	policy URLPolicy
}

// WithURLPolicy sets which upstream hosts providers may name. The zero policy still denies
// loopback, link-local and metadata addresses and names without a dot.
func (s *Service) WithURLPolicy(p URLPolicy) *Service {
	s.policy = p
	return s
}

// New returns a Service. keys may be nil; providers with a key then cannot be written.
func New(pool *pgxpool.Pool, keys Keys, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: pool, keys: keys, log: log}
}

// tx runs fn in the transaction of an audited change: events go into b, and the audit module writes
// them in lock order just before commit (ADR 0100 decision 1.7).
func (s *Service) tx(ctx context.Context, fn func(q *store.Queries, b audit.Batch) error) error {
	_, err := audit.InTx(ctx, s.pool, fn)
	return err
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func mapErr(err error) error {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case isUnique(err):
		return ErrConflict
	}
	return err
}

func toProvider(p store.Provider) Provider {
	var offered []string
	for _, f := range modelformat.Offered(p.Type) {
		offered = append(offered, string(f))
	}
	return Provider{
		ID: p.ID.String(), Name: p.Name, Type: p.Type, URL: p.Url, HasKey: len(p.ApiKey) > 0,
		AnthropicWorkspaceID: p.AnthropicWorkspaceID.String, Formats: offered,
		Source: string(p.Source), ManagedBySecret: p.ManagedBySecret.String, DisabledReason: p.DisabledReason.String,
		Removed: p.RemovedAt.Valid, CredentialLostAt: optTime(p.CredentialLostAt), AdminDisabled: outward.FromRow(p.AdminDisabledAt, p.AdminDisabledBy, p.AdminDisabledNote), CreatedAt: p.CreatedAt.Time, UpdatedAt: p.UpdatedAt.Time,
		HasCostKey: len(p.CostApiKey) > 0, CostScope: p.CostScope.String, CostKeyLostAt: optTime(p.CostApiKeyLostAt),
		Serving: Serving{State: ServingSaved},
	}
}

// optTime is t, or nil when it is NULL.
func optTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

func toModel(m store.Model) Model {
	out := Model{
		ID: m.ID.String(), ProviderID: m.ProviderID.String(), Name: m.Name, Guard: m.Guard,
		Format: m.Format, FormatOverridden: m.FormatOverridden, Source: string(m.Source), ManagedBySecret: m.ManagedBySecret.String, DisabledReason: m.DisabledReason.String,
		CreatedAt: m.CreatedAt.Time, UpdatedAt: m.UpdatedAt.Time,
	}
	return out
}

// ValidateProvider checks the fields of a provider.
func ValidateProvider(name, typ, rawURL string) error {
	if !namePattern.MatchString(name) {
		return invalid("name must be 1 to 64 characters of a-z, 0-9 and '-', starting and ending with a letter or digit")
	}
	return validateTypeURL(typ, rawURL)
}

func validateTypeURL(typ, rawURL string) error {
	if !typePattern.MatchString(typ) {
		return invalid("type must be an agentgateway provider name such as openai, openrouter or ollama")
	}
	u, err := url.Parse(rawURL)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return invalid("url must be an http(s) URL of an OpenAI-compatible API, for example https://openrouter.ai/api/v1")
	case u.User != nil:
		return invalid("url must not carry credentials; put the key in the API key field")
	case u.RawQuery != "" || u.Fragment != "":
		return invalid("url must not have a query or fragment")
	case len(rawURL) > 2048:
		return invalid("url is longer than 2048 characters")
	case strings.Contains(rawURL, "$"):
		return invalid("url must not contain '$': the gateway's config loader reads $NAME as an environment variable")
	}
	return nil
}

// ValidateWorkspaceID checks a provider's Anthropic workspace ID: empty, or wrkspc_ followed by
// letters and digits on a provider of type anthropic.
func ValidateWorkspaceID(typ, id string) error {
	switch {
	case id == "":
		return nil
	case typ != WorkspaceProviderType:
		return invalid("the Anthropic workspace ID exists for %s providers only", WorkspaceProviderType)
	case !modelformat.ValidWorkspaceID(id):
		return invalid("anthropic workspace ID must be wrkspc_ followed by letters and digits")
	}
	return nil
}

// ValidateModelFormat checks a model's format override against its provider's type; "" is the
// default and always valid.
func ValidateModelFormat(providerType, format string) error {
	if format == "" {
		return nil
	}
	if err := modelformat.Validate(providerType, modelformat.Format(format)); err != nil {
		return invalid("%s", err.Error())
	}
	return nil
}

// ValidateModel checks a model's name and guard against its provider type.
func ValidateModel(name string, guard bool, providerType string) error {
	if !modelPattern.MatchString(name) {
		return invalid("model name must be 1 to 256 characters of letters, digits and _ . : / @ -")
	}
	if guard && providerType != GuardProviderType {
		return invalid("the routing guard exists for %s providers only", GuardProviderType)
	}
	return nil
}

func providerSummary(p store.Provider) map[string]any {
	return map[string]any{
		"name": p.Name, "type": p.Type, "url": p.Url, "hasKey": len(p.ApiKey) > 0, "source": string(p.Source),
		"anthropicWorkspaceId": p.AnthropicWorkspaceID.String, "hasCostKey": len(p.CostApiKey) > 0, "costScope": p.CostScope.String,
	}
}

func modelSummary(m store.Model) map[string]any {
	out := map[string]any{
		"name": m.Name, "guard": m.Guard, "providerId": m.ProviderID.String(), "source": string(m.Source),
		"format": m.Format, "formatOverridden": m.FormatOverridden,
	}
	return out
}

func (s *Service) seal(org, id pgtype.UUID, key string) ([]byte, error) {
	if key == "" {
		return nil, nil
	}
	if s.keys == nil {
		return nil, ErrNoKeys
	}
	env, err := s.keys.Seal(KeyBinding(org, id), []byte(key))
	if err != nil {
		return nil, fmt.Errorf("catalog: seal provider key: %w", err)
	}
	return marshalEnvelope(env)
}

func newID() (pgtype.UUID, error) {
	id, err := uuid.NewV7()
	return pgtype.UUID{Bytes: id, Valid: true}, err
}
