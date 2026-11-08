// SPDX-License-Identifier: Apache-2.0

// Package mcpservers manages an org's remote MCP servers and the tier of each of their tools
// (ADR 0057, plan item MC1).
//
// One service layer serves every caller: the UI's connect-rpc service, the admin API, the
// declarative-Secret importer and the reconciler's prober. Each change is validated, written and
// audited in one transaction. A server's static credential is sealed with the secret store
// (internal/secretstore) and never leaves this package in the clear; only the reconciler opens it
// (OpenCredential) to deliver it to the gateway.
//
// A server is either entered by a person (source "ui") or declared by a labelled Kubernetes Secret
// (source "secret", read-only here, see package declarative), under the same rules as model
// providers (package catalog): a declared server wins a name collision, and deleting the Secret
// disables the server without deleting the row.
//
// # Tool tiers
//
// Every tool a server lists enters the "ask" tier: a call parks the run and a human approves the
// exact call (ADR 0008, 0042). An admin moves a tool to "auto" (the agent calls it directly through
// the gateway) or "never". A tool's readOnlyHint is only a suggestion. A tool that appears later
// enters the ask tier. A known tool whose description or input schema changes keeps its tier and is
// flagged for review, because a changed description is how a server can try to steer an agent
// (prompt injection).
package mcpservers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpsanitize"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/sinkclass"
)

// Errors are the catalog's, so every caller maps them the same way. Messages are safe to show.
var (
	ErrNotFound = catalog.ErrNotFound
	ErrReadOnly = catalog.ErrReadOnly
	ErrConflict = catalog.ErrConflict
	ErrNoKeys   = catalog.ErrNoKeys
)

// ErrToolChanged is returned when a tool's declaration is set against a content hash that is no longer
// the tool's: the server republished it since the admin read it. Callers map it to a conflict (Aborted,
// HTTP 409). Message safe to show.
var ErrToolChanged = errors.New("the tool changed since it was read; read it again and review the new description")

// InvalidError is a validation failure.
type InvalidError = catalog.InvalidError

func invalid(format string, a ...any) error { return &InvalidError{Msg: fmt.Sprintf(format, a...)} }

func readOnly(secret string) error {
	return fmt.Errorf("%w (managed by the Secret %s; change it there)", ErrReadOnly, secret)
}

// Keys seals and opens secret values; *secretstore.Keyring is one.
type Keys = catalog.Keys

// Table and column of server credentials, as bound into the AAD.
const (
	ServersTable     = "mcp_servers"
	CredentialColumn = "credential"
)

// CredentialBinding is the AAD binding of a server's credential.
func CredentialBinding(org, id pgtype.UUID) secretstore.Binding {
	return secretstore.Binding{OrgID: org.String(), Table: ServersTable, RowID: id.String(), Column: CredentialColumn}
}

// Sources.
const (
	SourceUI     = catalog.SourceUI
	SourceSecret = catalog.SourceSecret
)

// Transports.
const (
	TransportStreamableHTTP = "streamable-http"
	TransportSSE            = "sse"
)

// Tiers.
const (
	TierAsk   = "ask"
	TierAuto  = "auto"
	TierNever = "never"
)

// Health values.
const (
	HealthUnknown   = "unknown"
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
)

// FailureThreshold is how many probes in a row must fail before a server is unhealthy.
const FailureThreshold = 3

// Reasons a server is disabled.
const (
	ReasonShadowed = "shadowed by an MCP server of the same name declared in a Kubernetes Secret"
	// ReasonRemoved marks a declared server whose Secret was deleted. The row stays; restoring the
	// Secret brings it back, and an admin may delete it.
	ReasonRemoved = "its Kubernetes Secret was deleted; restore the Secret or delete this entry"
)

// Review reasons. A changed auto tool drops to the ask tier until an admin reviews it.
const (
	ReviewChanged        = "the tool's description or input schema changed since an admin last reviewed it"
	ReviewChangedDemoted = "the tool's description or input schema changed since an admin last reviewed it; it was moved from auto to ask until you review it"
)

// MaxToolNameLen is what model APIs accept of a tool name. Agents see
// "mcp__koinon__<org>-<server>-ask_<tool>" at most (the longest harness prefix, the gateway's target
// prefix and, for ask-tier tools, the -ask suffix), made of [A-Za-z0-9_-].
const (
	MaxToolNameLen   = 64
	harnessPrefixLen = len("mcp__koinon__")
)

// ToolNameProblem says why the tool cannot be offered to agents under its name, or "" if it can. A
// tool is judged by its longest form (the ask target), so a tier change never changes the verdict.
func ToolNameProblem(orgSlug, server, tool string) string {
	for _, r := range tool {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return "unavailable: the tool's name has characters (such as '.') that model APIs do not accept"
		}
	}
	if n := harnessPrefixLen + len(AskTargetName(orgSlug, server)) + 1 + len(tool); n > MaxToolNameLen {
		return fmt.Sprintf("unavailable: name too long (%d characters with the prefixes agents see; models accept %d)", n, MaxToolNameLen)
	}
	return ""
}

// NameMaxLen is the longest server name: the gateway target is "<org slug>-<name>" and a target
// name is at most 63 characters, an org slug at most 40.
const NameMaxLen = 24

var (
	namePattern   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,22}[a-z0-9])?$`)
	headerPattern = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]{1,128}$`)
	prefixPattern = regexp.MustCompile(`^[ -~]{0,32}$`)
	toolPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	// reservedHeaders cannot carry a credential: Koinon, HTTP or MCP uses them.
	reservedHeaders = []string{
		"host", "content-length", "content-type", "accept", "connection", "transfer-encoding", "upgrade",
		"te", "trailer", "mcp-session-id", "mcp-protocol-version", "mcp-method", "x-koinon-agent", "x-koinon-org",
	}
)

// AskStatusTool is the tool of an ask-tier target that reports the state of an approval request. A
// server tool of this name is not offered in the ask tier.
const AskStatusTool = "get_approval_status"

// AskSuffix ends the name of the gateway target that offers a server's ask-tier tools.
const AskSuffix = "-ask"

// TargetName is the gateway target of an org's server.
func TargetName(orgSlug, server string) string { return orgSlug + "-" + server }

// AskTargetName is the gateway target that offers the ask-tier tools of an org's server.
func AskTargetName(orgSlug, server string) string { return TargetName(orgSlug, server) + AskSuffix }

// Server is a server as callers see it: never its credential.
type Server struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	URL              string `json:"url"`
	Transport        string `json:"transport"`
	HasCredential    bool   `json:"hasCredential"`
	CredentialHeader string `json:"credentialHeader,omitempty"`
	CredentialPrefix string `json:"credentialPrefix,omitempty"`
	Source           string `json:"source"`
	ManagedBySecret  string `json:"managedBySecret,omitempty"`
	DisabledReason   string `json:"disabledReason,omitempty"`
	// Removed is set when the declaring Secret was deleted.
	Removed bool `json:"removed,omitempty"`
	// CredentialLostAt is set when the credential was lost with the KEK that encrypted it (keys
	// forget, ADR 0085 decision 13): the server is not served until the credential is entered again.
	// CredentialHeader and CredentialPrefix are kept.
	CredentialLostAt *time.Time `json:"credentialLostAt,omitempty"`
	// Kind and DataClass are what the server declares it exposes (ADR 0084 decision 8): its kind and
	// the legs it carries, in the order of Legs. A server nobody classified is DefaultDeclaration.
	Kind      string   `json:"kind"`
	DataClass []string `json:"dataClass"`
	// CodeHostConnectionID is the code-host connection the server acts through, "" for none. The
	// code-host derivation of its tools looks a repository up in that connection's list only (ADR 0091).
	CodeHostConnectionID string     `json:"codeHostConnectionId"`
	Health               string     `json:"health"`
	HealthReason         string     `json:"healthReason,omitempty"`
	HealthCheckedAt      *time.Time `json:"healthCheckedAt,omitempty"`
	HealthSince          *time.Time `json:"healthSince,omitempty"`
	ToolsSyncedAt        *time.Time `json:"toolsSyncedAt,omitempty"`
	CreatedAt            time.Time  `json:"createdAt"`
	UpdatedAt            time.Time  `json:"updatedAt"`
}

// Tool is a tool of a server.
type Tool struct {
	ServerID    string `json:"serverId"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// ReadOnlyHint is the server's own claim; an admin decides the tier.
	ReadOnlyHint bool   `json:"readOnlyHint"`
	Tier         string `json:"tier"`
	ReviewReason string `json:"reviewReason,omitempty"`
	// RequiresTwoApprovers makes a call in the ask tier need two approvers.
	RequiresTwoApprovers bool `json:"requiresTwoApprovers,omitempty"`
	// ContentHash identifies the description and schema as they were last published.
	ContentHash string `json:"contentHash"`
	// DescriptionOriginalLength is the length in characters of the description as the server published
	// it. Description is the sanitised text (internal/mcpsanitize); Capped and Cleaned say it was cut
	// or had characters removed or tags escaped, in the description or in the input schema's strings.
	DescriptionOriginalLength int  `json:"descriptionOriginalLength"`
	DescriptionCapped         bool `json:"descriptionCapped"`
	DescriptionCleaned        bool `json:"descriptionCleaned"`
	// Unavailable says why agents cannot be offered the tool, for example a name too long.
	Unavailable string     `json:"unavailable,omitempty"`
	Present     bool       `json:"present"`
	FirstSeenAt time.Time  `json:"firstSeenAt"`
	LastSeenAt  time.Time  `json:"lastSeenAt"`
	TierSetBy   string     `json:"tierSetBy,omitempty"`
	TierSetAt   *time.Time `json:"tierSetAt,omitempty"`
	// Sink is the tool's declaration for the leak guard (ADR 0091): its sink class, its address
	// arguments and what its results carry. A tool nobody declared has sinkclass.Default.
	Sink sinkclass.Declaration `json:"sink"`
}

// Service is the MCP server service layer.
type Service struct {
	pool      *pgxpool.Pool
	keys      Keys
	log       *slog.Logger
	policy    catalog.URLPolicy
	preflight Preflight
}

// Preflight inspects a server before it is saved. It returns an *InvalidError to refuse it, for
// example because the server needs per-user OAuth.
type Preflight func(ctx context.Context, p PreflightRequest) error

// PreflightRequest is what Preflight may use. The secret is not part of it: the add-time check is
// anonymous (see Diagnose).
type PreflightRequest struct {
	URL       string
	Transport string
}

// New returns a Service. keys may be nil; servers with a credential then cannot be written.
func New(pool *pgxpool.Pool, keys Keys, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: pool, keys: keys, log: log}
}

// WithURLPolicy sets which hosts servers may name. The zero policy still denies loopback,
// link-local and metadata addresses and names without a dot.
func (s *Service) WithURLPolicy(p catalog.URLPolicy) *Service {
	s.policy = p
	return s
}

// WithPreflight sets the check run before a server is created or its URL or credential changes.
func (s *Service) WithPreflight(p Preflight) *Service {
	s.preflight = p
	return s
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
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23503" && strings.Contains(pg.ConstraintName, "code_host_connection_id") {
		return invalid("codeHostConnectionId is not a connection of this org")
	}
	return err
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func toServer(r store.McpServer) Server {
	return Server{
		ID: r.ID.String(), Name: r.Name, URL: r.Url, Transport: string(r.Transport),
		HasCredential: len(r.Credential) > 0, CredentialHeader: r.CredentialHeader.String, CredentialPrefix: r.CredentialPrefix,
		Source: string(r.Source), ManagedBySecret: r.ManagedBySecret.String, DisabledReason: r.DisabledReason.String,
		Removed: r.RemovedAt.Valid, CredentialLostAt: timePtr(r.CredentialLostAt), Health: string(r.Health), HealthReason: r.HealthReason.String,
		HealthCheckedAt: timePtr(r.HealthCheckedAt), HealthSince: timePtr(r.HealthSince), ToolsSyncedAt: timePtr(r.ToolsSyncedAt),
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
		Kind: declarationOf(r).Kind, DataClass: declarationOf(r).Legs, CodeHostConnectionID: declarationOf(r).CodeHostConnection,
	}
}

func toTool(t store.McpTool) Tool {
	return Tool{
		ServerID: t.ServerID.String(), Name: t.Name, Description: t.Description, ReadOnlyHint: t.ReadOnlyHint,
		Tier: string(t.Tier), ReviewReason: t.ReviewReason.String, Present: t.Present,
		RequiresTwoApprovers: t.RequiresTwoApprovers, ContentHash: t.ContentHash,
		DescriptionOriginalLength: int(t.DescriptionOriginalLength), DescriptionCapped: t.DescriptionCapped, DescriptionCleaned: t.DescriptionCleaned,
		FirstSeenAt: t.FirstSeenAt.Time, LastSeenAt: t.LastSeenAt.Time, TierSetBy: t.TierSetBy.String, TierSetAt: timePtr(t.TierSetAt),
		Sink: sinkOf(t),
	}
}

func serverSummary(r store.McpServer) map[string]any {
	return map[string]any{
		"name": r.Name, "url": r.Url, "transport": string(r.Transport), "hasCredential": len(r.Credential) > 0,
		"credentialHeader": r.CredentialHeader.String, "source": string(r.Source),
		"kind": declarationOf(r).Kind, "dataClass": declarationOf(r).Legs,
	}
}

// ValidateName checks a server's name.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return invalid("name must be 1 to %d characters of a-z, 0-9 and '-', starting and ending with a letter or digit", NameMaxLen)
	}
	if name == "ask" {
		return invalid("name must not be %q, which the gateway uses for the approval-gated targets", name)
	}
	if strings.HasSuffix(name, AskSuffix) {
		return invalid("name must not end in %q, which the gateway uses for the approval-gated tools of a server", AskSuffix)
	}
	return nil
}

// ValidateRemote checks a server's URL, transport and credential header and prefix.
func ValidateRemote(rawURL, transport, header, prefix string) error {
	u, err := url.Parse(rawURL)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return invalid("url must be an http(s) URL of a remote MCP server, for example https://mcp.example.com/mcp")
	case u.User != nil:
		return invalid("url must not carry credentials; put them in the credential")
	case u.RawQuery != "" || u.Fragment != "":
		return invalid("url must not have a query or fragment, which could carry a secret; use a credential header")
	case len(rawURL) > 2048:
		return invalid("url is longer than 2048 characters")
	}
	if transport != TransportStreamableHTTP && transport != TransportSSE {
		return invalid("transport must be %q or %q", TransportStreamableHTTP, TransportSSE)
	}
	if header != "" {
		if !headerPattern.MatchString(header) {
			return invalid("credential header must be an HTTP header name")
		}
		if slices.Contains(reservedHeaders, strings.ToLower(header)) {
			return invalid("credential header %q is used by HTTP or MCP and cannot carry a credential", header)
		}
	}
	if !prefixPattern.MatchString(prefix) {
		return invalid("credential prefix must be at most 32 printable ASCII characters, for example \"Bearer \"")
	}
	return nil
}

func newID() (pgtype.UUID, error) {
	id, err := uuid.NewV7()
	return pgtype.UUID{Bytes: id, Valid: true}, err
}

func parseID(s string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if err := id.Scan(s); err != nil || !id.Valid {
		return id, ErrNotFound // an ID that is no UUID names nothing
	}
	return id, nil
}

func (s *Service) seal(org, id pgtype.UUID, value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	if s.keys == nil {
		return nil, ErrNoKeys
	}
	env, err := s.keys.Seal(CredentialBinding(org, id), []byte(value))
	if err != nil {
		return nil, fmt.Errorf("mcpservers: seal credential: %w", err)
	}
	return json.Marshal(env)
}

// OpenCredential decrypts a server's credential, or returns "" for a server without one. Only the
// reconciler calls it, to deliver credentials to the gateway.
func OpenCredential(keys catalog.Opener, org, id pgtype.UUID, envelope []byte) (string, error) {
	if len(envelope) == 0 {
		return "", nil
	}
	if keys == nil {
		return "", ErrNoKeys
	}
	var env secretstore.Envelope
	if err := json.Unmarshal(envelope, &env); err != nil {
		return "", invalid("server credential envelope is unreadable")
	}
	plain, err := keys.Open(CredentialBinding(org, id), env)
	if err != nil {
		return "", err
	}
	defer clear(plain)
	return string(plain), nil
}

// Input creates a server.
type Input struct {
	Name      string
	URL       string
	Transport string
	// CredentialValue is the secret; empty means the server needs none. CredentialHeader defaults
	// to Authorization. CredentialPrefix is what precedes the secret in the header ("Bearer ").
	CredentialHeader string
	CredentialPrefix string
	CredentialValue  string
	// Class is what the server exposes. Nil is DefaultDeclaration: not yet classified.
	Class *Declaration
}

// Patch changes a server; nil fields stay.
type Patch struct {
	URL       *string
	Transport *string
	// Credential replaces the credential; RemoveCredential removes it. Setting both is invalid.
	Credential       *CredentialPatch
	RemoveCredential bool
}

// CredentialPatch replaces the header, prefix or secret. Value nil keeps the stored secret.
type CredentialPatch struct {
	Header string
	Prefix string
	Value  *string
}

func normalize(in *Input) {
	if in.Transport == "" {
		in.Transport = TransportStreamableHTTP
	}
	if in.CredentialValue != "" && in.CredentialHeader == "" {
		in.CredentialHeader = "Authorization"
	}
}

// List returns the org's servers, declared and entered.
func (s *Service) List(ctx context.Context, org pgtype.UUID) ([]Server, error) {
	rows, err := store.New(s.pool).ListMCPServers(ctx, org)
	if err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(rows))
	for _, r := range rows {
		out = append(out, toServer(r))
	}
	return out, nil
}

// Get returns one server.
func (s *Service) Get(ctx context.Context, org pgtype.UUID, id string) (Server, error) {
	sid, err := parseID(id)
	if err != nil {
		return Server{}, err
	}
	row, err := store.New(s.pool).GetMCPServer(ctx, store.GetMCPServerParams{OrgID: org, ID: sid})
	if err != nil {
		return Server{}, mapErr(err)
	}
	return toServer(row), nil
}

// Create adds a server entered by a person.
func (s *Service) Create(ctx context.Context, org pgtype.UUID, actor audit.Actor, creator pgtype.UUID, in Input) (Server, error) {
	normalize(&in)
	if err := ValidateName(in.Name); err != nil {
		return Server{}, err
	}
	if err := ValidateRemote(in.URL, in.Transport, in.CredentialHeader, in.CredentialPrefix); err != nil {
		return Server{}, err
	}
	if in.Class != nil {
		d, err := in.Class.Normalize()
		if err != nil {
			return Server{}, err
		}
		in.Class = &d
	}
	if err := s.policy.Check(ctx, in.URL); err != nil {
		return Server{}, err
	}
	if err := s.runPreflight(ctx, in.URL, in.Transport); err != nil {
		return Server{}, err
	}
	id, err := newID()
	if err != nil {
		return Server{}, err
	}
	cred, err := s.seal(org, id, in.CredentialValue)
	if err != nil {
		return Server{}, err
	}
	header := pgtype.Text{String: in.CredentialHeader, Valid: len(cred) > 0}
	var row store.McpServer
	err = s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		p := store.CreateMCPServerParams{
			OrgID: org, ID: id, Name: in.Name, Url: in.URL, Transport: store.McpTransport(in.Transport),
			CredentialHeader: header, CredentialPrefix: prefixIf(in.CredentialPrefix, len(cred) > 0), Credential: cred,
			Source: store.ConfigSourceUi, CreatedBy: creator,
		}
		createClass(&p, in.Class)
		if in.Class != nil {
			link, err := lockLinkTarget(ctx, q, org, *in.Class)
			if err != nil {
				return err
			}
			if err := link.check(in.URL); err != nil {
				return err
			}
		}
		row, err = q.CreateMCPServer(ctx, p)
		if err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "mcp_server.create", TargetKind: "mcp_server", TargetID: id.String(), After: serverSummary(row),
		})
		return nil
	})
	if err != nil {
		return Server{}, mapErr(err)
	}
	return toServer(row), nil
}

func prefixIf(prefix string, has bool) string {
	if !has {
		return ""
	}
	return prefix
}

func (s *Service) runPreflight(ctx context.Context, rawURL, transport string) error {
	if s.preflight == nil {
		return nil
	}
	return s.preflight(ctx, PreflightRequest{URL: rawURL, Transport: transport})
}

// Update changes a server entered by a person.
func (s *Service) Update(ctx context.Context, org pgtype.UUID, actor audit.Actor, id string, patch Patch) (Server, error) {
	sid, err := parseID(id)
	if err != nil {
		return Server{}, err
	}
	if patch.Credential != nil && patch.RemoveCredential {
		return Server{}, invalid("set a credential or remove it, not both")
	}
	var out store.McpServer
	err = s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		cur, err := q.GetMCPServer(ctx, store.GetMCPServerParams{OrgID: org, ID: sid})
		if err != nil {
			return err
		}
		if cur.Source != store.ConfigSourceUi {
			return readOnly(cur.ManagedBySecret.String)
		}
		u, transport := cur.Url, string(cur.Transport)
		if patch.URL != nil {
			u = *patch.URL
		}
		if patch.Transport != nil {
			transport = *patch.Transport
		}
		header, prefix, cred := cur.CredentialHeader.String, cur.CredentialPrefix, cur.Credential
		value := ""
		switch {
		case patch.RemoveCredential:
			header, prefix, cred = "", "", nil
		case patch.Credential != nil:
			header, prefix = patch.Credential.Header, patch.Credential.Prefix
			if header == "" {
				header = "Authorization"
			}
			if patch.Credential.Value != nil {
				value = *patch.Credential.Value
				if cred, err = s.seal(org, sid, value); err != nil {
					return err
				}
			}
			if len(cred) == 0 {
				return invalid("a credential needs a value")
			}
		}
		if err := ValidateRemote(u, transport, header, prefix); err != nil {
			return err
		}
		if err := s.policy.Check(ctx, u); err != nil {
			return err
		}
		if u != cur.Url || transport != string(cur.Transport) {
			if err := s.runPreflight(ctx, u, transport); err != nil {
				return err
			}
		}
		out, err = q.UpdateMCPServer(ctx, store.UpdateMCPServerParams{
			OrgID: org, ID: sid, Url: u, Transport: store.McpTransport(transport),
			CredentialHeader: pgtype.Text{String: header, Valid: len(cred) > 0}, CredentialPrefix: prefixIf(prefix, len(cred) > 0),
			// The envelope is rewritten only when this patch replaces or removes it. cur was read
			// without a lock, and a concurrent `keys rewrap` or `reencrypt` may have replaced the
			// envelope since (the preflight above widens that window); writing cur's back would undo it.
			SetCredentialMeta: patch.Credential != nil || patch.RemoveCredential,
			SetCredential:     patch.RemoveCredential || (patch.Credential != nil && patch.Credential.Value != nil), Credential: cred,
		})
		if err != nil {
			return err
		}
		after := serverSummary(out)
		after["credentialChanged"] = patch.Credential != nil || patch.RemoveCredential
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "mcp_server.update", TargetKind: "mcp_server", TargetID: id,
			Before: serverSummary(cur), After: after,
		})
		return resetOnURLChange(ctx, q, b, org, actor, cur, out)
	})
	if err != nil {
		return Server{}, mapErr(err)
	}
	return toServer(out), nil
}

// Delete removes a server entered by a person, with its tools.
func (s *Service) Delete(ctx context.Context, org pgtype.UUID, actor audit.Actor, id string) error {
	sid, err := parseID(id)
	if err != nil {
		return err
	}
	err = s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		cur, err := q.GetMCPServer(ctx, store.GetMCPServerParams{OrgID: org, ID: sid})
		if err != nil {
			return err
		}
		// A declared server whose Secret is gone may be deleted for good; a live one only changes
		// in its Secret.
		if cur.Source != store.ConfigSourceUi && !cur.RemovedAt.Valid {
			return readOnly(cur.ManagedBySecret.String)
		}
		if _, err := q.DeleteMCPServer(ctx, store.DeleteMCPServerParams{OrgID: org, ID: sid}); err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "mcp_server.delete", TargetKind: "mcp_server", TargetID: id, Before: serverSummary(cur),
		})
		return nil
	})
	return mapErr(err)
}

// Tools returns the tools of a server (all of the org's when serverID is empty).
func (s *Service) Tools(ctx context.Context, org pgtype.UUID, serverID string) ([]Tool, error) {
	q := store.New(s.pool)
	var rows []store.McpTool
	if serverID == "" {
		var err error
		if rows, err = q.ListMCPTools(ctx, org); err != nil {
			return nil, err
		}
	} else {
		sid, err := parseID(serverID)
		if err != nil {
			return nil, err
		}
		if _, err := q.GetMCPServer(ctx, store.GetMCPServerParams{OrgID: org, ID: sid}); err != nil {
			return nil, mapErr(err)
		}
		if rows, err = q.ListMCPToolsOfServer(ctx, store.ListMCPToolsOfServerParams{OrgID: org, ServerID: sid}); err != nil {
			return nil, err
		}
	}
	slug, err := q.GetOrgSlug(ctx, org)
	if err != nil {
		return nil, mapErr(err)
	}
	names := map[pgtype.UUID]string{}
	servers, err := q.ListMCPServers(ctx, org)
	if err != nil {
		return nil, err
	}
	for _, sv := range servers {
		names[sv.ID] = sv.Name
	}
	out := make([]Tool, 0, len(rows))
	for _, r := range rows {
		t := toTool(r)
		t.Unavailable = ToolNameProblem(slug, names[r.ServerID], r.Name)
		out = append(out, t)
	}
	return out, nil
}

// SetTwoApprovers turns the two-approver requirement of a tool on or off. It is off by default and
// applies to calls in the ask tier. Works on declared servers too.
func (s *Service) SetTwoApprovers(ctx context.Context, org pgtype.UUID, actor audit.Actor, serverID, tool string, required bool) (Tool, error) {
	sid, err := parseID(serverID)
	if err != nil {
		return Tool{}, err
	}
	var out store.McpTool
	err = s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		srv, err := q.GetMCPServer(ctx, store.GetMCPServerParams{OrgID: org, ID: sid})
		if err != nil {
			return err
		}
		cur, err := q.GetMCPTool(ctx, store.GetMCPToolParams{OrgID: org, ServerID: sid, Name: tool})
		if err != nil {
			return err
		}
		out, err = q.SetMCPToolTwoApprovers(ctx, store.SetMCPToolTwoApproversParams{OrgID: org, ServerID: sid, Name: tool, Required: required})
		if err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "mcp_tool.two_approvers", TargetKind: "mcp_tool", TargetID: serverID + "/" + tool,
			Before: map[string]any{"server": srv.Name, "tool": tool, "required": cur.RequiresTwoApprovers},
			After:  map[string]any{"server": srv.Name, "tool": tool, "required": required},
		})
		return nil
	})
	if err != nil {
		return Tool{}, mapErr(err)
	}
	return toTool(out), nil
}

// SetTier moves a tool to a tier. It works on declared servers too: tiers are decisions about
// risk, not part of what a Secret declares. It clears the tool's review flag.
func (s *Service) SetTier(ctx context.Context, org pgtype.UUID, actor audit.Actor, serverID, tool, tier string) (Tool, error) {
	if tier != TierAsk && tier != TierAuto && tier != TierNever {
		return Tool{}, invalid("tier must be %q, %q or %q", TierAsk, TierAuto, TierNever)
	}
	sid, err := parseID(serverID)
	if err != nil {
		return Tool{}, err
	}
	var out store.McpTool
	err = s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		srv, err := q.GetMCPServer(ctx, store.GetMCPServerParams{OrgID: org, ID: sid})
		if err != nil {
			return err
		}
		cur, err := q.GetMCPTool(ctx, store.GetMCPToolParams{OrgID: org, ServerID: sid, Name: tool})
		if err != nil {
			return err
		}
		out, err = q.SetMCPToolTier(ctx, store.SetMCPToolTierParams{
			OrgID: org, ServerID: sid, Name: tool, Tier: store.McpTier(tier), SetBy: pgtype.Text{String: actor.String(), Valid: true},
		})
		if err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "mcp_tool.tier", TargetKind: "mcp_tool", TargetID: serverID + "/" + tool,
			Before: map[string]any{"server": srv.Name, "tool": tool, "tier": string(cur.Tier), "reviewFlag": cur.ReviewReason.Valid},
			After:  map[string]any{"server": srv.Name, "tool": tool, "tier": tier},
		})
		return nil
	})
	if err != nil {
		return Tool{}, mapErr(err)
	}
	return toTool(out), nil
}

// AcknowledgeReview clears a tool's review flag without changing its tier.
func (s *Service) AcknowledgeReview(ctx context.Context, org pgtype.UUID, actor audit.Actor, serverID, tool string) (Tool, error) {
	sid, err := parseID(serverID)
	if err != nil {
		return Tool{}, err
	}
	var out store.McpTool
	err = s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		srv, err := q.GetMCPServer(ctx, store.GetMCPServerParams{OrgID: org, ID: sid})
		if err != nil {
			return err
		}
		out, err = q.AcknowledgeMCPToolReview(ctx, store.AcknowledgeMCPToolReviewParams{OrgID: org, ServerID: sid, Name: tool})
		if err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "mcp_tool.review", TargetKind: "mcp_tool", TargetID: serverID + "/" + tool,
			After: map[string]any{"server": srv.Name, "tool": tool, "tier": string(out.Tier)},
		})
		return nil
	})
	if err != nil {
		return Tool{}, mapErr(err)
	}
	return toTool(out), nil
}

// ListedTool is a tool as a server published it.
type ListedTool struct {
	Name         string
	Description  string
	InputSchema  json.RawMessage
	ReadOnlyHint bool
}

// ContentHash is the SHA-256 (hex) of a tool's description and canonical input schema.
func ContentHash(description string, schema json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte(description))
	h.Write([]byte{0})
	h.Write(canonicalJSON(schema))
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalJSON re-encodes a JSON document with sorted keys and no insignificant space.
func canonicalJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return b
}

// SyncResult says what a sync changed.
type SyncResult struct {
	New     []string
	Changed []string
	// Demoted are the changed tools that were in the auto tier and moved to ask.
	Demoted []string
	Absent  []string
}

// SyncTools records the tools a server listed. New tools enter the ask tier, known tools whose
// description or input schema changed are flagged for review (and keep their tier), tools the server
// no longer lists are marked absent. It is what the prober calls after a successful tools/list.
func (s *Service) SyncTools(ctx context.Context, org pgtype.UUID, serverID pgtype.UUID, listed []ListedTool) (SyncResult, error) {
	var res SyncResult
	err := s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		srv, err := q.GetMCPServer(ctx, store.GetMCPServerParams{OrgID: org, ID: serverID})
		if err != nil {
			return err
		}
		known, err := q.ListMCPToolsOfServer(ctx, store.ListMCPToolsOfServerParams{OrgID: org, ServerID: serverID})
		if err != nil {
			return err
		}
		have := map[string]store.McpTool{}
		for _, t := range known {
			have[t.Name] = t
		}
		var names []string
		for _, lt := range listed {
			if !toolPattern.MatchString(lt.Name) {
				s.log.Warn("an MCP server lists a tool with an unusable name; ignoring it", "server", srv.Name)
				continue
			}
			schema := lt.InputSchema
			if len(schema) == 0 || !json.Valid(schema) || schema[0] != '{' {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			// The description and the schema's strings reach the model verbatim, so what is stored,
			// hashed and offered is the sanitised text (ADR 0057 amendment, #52).
			desc, rep := mcpsanitize.Description(lt.Description)
			schema, srep := mcpsanitize.Schema(schema)
			rep.Capped, rep.Cleaned = rep.Capped || srep.Capped, rep.Cleaned || srep.Cleaned
			names = append(names, lt.Name)
			hash := ContentHash(desc, schema)
			cur, ok := have[lt.Name]
			switch {
			case !ok:
				if _, err := q.InsertMCPTool(ctx, store.InsertMCPToolParams{
					OrgID: org, ServerID: serverID, Name: lt.Name, Description: desc, InputSchema: schema, ContentHash: hash, ReadOnlyHint: lt.ReadOnlyHint,
					DescriptionOriginalLength: int64(rep.OriginalLen), DescriptionCapped: rep.Capped, DescriptionCleaned: rep.Cleaned,
				}); err != nil {
					return err
				}
				res.New = append(res.New, lt.Name)
			case cur.ContentHash != hash:
				reason := ReviewChanged
				if cur.Tier == store.McpTierAuto {
					reason = ReviewChangedDemoted
					res.Demoted = append(res.Demoted, lt.Name)
				}
				out, err := q.UpdateMCPToolContent(ctx, store.UpdateMCPToolContentParams{
					OrgID: org, ServerID: serverID, Name: lt.Name, Description: desc, InputSchema: schema, ContentHash: hash,
					ReadOnlyHint: lt.ReadOnlyHint, ReviewReason: pgtype.Text{String: reason, Valid: true},
					DescriptionOriginalLength: int64(rep.OriginalLen), DescriptionCapped: rep.Capped, DescriptionCleaned: rep.Cleaned,
				})
				if err != nil {
					return err
				}
				// The query put the declaration back to the default; the audit says what it was.
				if err := recordToolDeclarationReset(ctx, q, b, org, audit.System("koinon-reconciler:mcp-sync"), srv.Name, cur, out, ReasonContentChanged); err != nil {
					return err
				}
				res.Changed = append(res.Changed, lt.Name)
			default:
				if err := q.TouchMCPTool(ctx, store.TouchMCPToolParams{
					OrgID: org, ServerID: serverID, Name: lt.Name, ReadOnlyHint: lt.ReadOnlyHint,
					DescriptionOriginalLength: int64(rep.OriginalLen), DescriptionCapped: rep.Capped, DescriptionCleaned: rep.Cleaned,
				}); err != nil {
					return err
				}
			}
		}
		for _, t := range known {
			if t.Present && !slices.Contains(names, t.Name) {
				res.Absent = append(res.Absent, t.Name)
			}
		}
		if len(res.Absent) > 0 {
			if err := q.MarkMCPToolsAbsent(ctx, store.MarkMCPToolsAbsentParams{OrgID: org, ServerID: serverID, Listed: names}); err != nil {
				return err
			}
		}
		if len(res.New) == 0 && len(res.Changed) == 0 && len(res.Absent) == 0 {
			return nil
		}
		slices.Sort(res.New)
		slices.Sort(res.Changed)
		slices.Sort(res.Demoted)
		slices.Sort(res.Absent)
		b.Org(audit.Event{
			Org: org, Actor: audit.System("koinon-reconciler:mcp-sync"), Action: "mcp_tool.sync", TargetKind: "mcp_server", TargetID: serverID.String(),
			After: map[string]any{"server": srv.Name, "new": res.New, "changed": res.Changed, "demoted": res.Demoted, "absent": res.Absent},
		})
		return nil
	})
	return res, mapErr(err)
}

// RecordProbe stores one probe's result. A server turns unhealthy after FailureThreshold failures
// in a row and healthy again after one success; reason says what failed. synced says the probe's
// tool list was recorded (SyncTools). It returns the server and whether its health changed.
func (s *Service) RecordProbe(ctx context.Context, org, serverID pgtype.UUID, ok bool, reason string, synced bool) (Server, bool, error) {
	var out Server
	var changed bool
	err := s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		before, err := q.GetMCPServer(ctx, store.GetMCPServerParams{OrgID: org, ID: serverID})
		if err != nil {
			return err
		}
		if len(reason) > 500 {
			reason = reason[:500]
		}
		row, err := q.RecordMCPServerProbe(ctx, store.RecordMCPServerProbeParams{
			OrgID: org, ID: serverID, Ok: ok, Threshold: FailureThreshold, Reason: reason, Synced: synced,
		})
		if err != nil {
			return err
		}
		out = toServer(row)
		changed = before.Health != row.Health
		if !changed || before.Health == store.McpHealthUnknown && row.Health == store.McpHealthHealthy {
			return nil
		}
		b.Org(audit.Event{
			Org: org, Actor: audit.System("koinon-reconciler:mcp-sync"), Action: "mcp_server.health", TargetKind: "mcp_server", TargetID: serverID.String(),
			Before: map[string]any{"name": before.Name, "health": string(before.Health)},
			After:  map[string]any{"name": row.Name, "health": string(row.Health), "reason": row.HealthReason.String},
		})
		return nil
	})
	return out, changed, mapErr(err)
}
