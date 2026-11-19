// SPDX-License-Identifier: Apache-2.0

// Package remoteagents manages an org's remote agents: external A2A agents that Koinon agents may call
// (#1129, ADR 0123 decisions 9, 11 and 13). A remote agent is its own resource. It is not an MCP server.
//
// One service layer serves every caller: the connect-rpc service, the admin API and the reconciler's health
// check. Each change is validated, written and audited in one transaction. The credential is sealed with the
// secret store and never leaves this package in the clear except to the HTTP client of one card read
// (OpenCredential).
//
// # The pin
//
// A remote agent is pinned to its agent card: the SHA-256 of the canonical card (package a2aout). Only an
// admin moves the pin: at registration, on a change of the URL, or with AcceptCard. The health check reads
// the card again. A card that differs from the pin is stored as the observed card and makes the agent
// unhealthy. It is a finding, never an update. The card text is data from another party. It is for admins
// only (GetCard) and never reaches a model, a log or an error message.
package remoteagents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/a2aout"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/restorehold"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// Errors are the catalog's, so every caller maps them the same way. Messages are safe to show.
var (
	ErrNotFound = catalog.ErrNotFound
	ErrConflict = catalog.ErrConflict
	ErrNoKeys   = catalog.ErrNoKeys
)

// ErrCardMoved is returned by AcceptCard when the observed card is not the one the admin read: the health
// check saw another card since. Callers map it to a conflict.
var ErrCardMoved = fmt.Errorf("%w: the observed card changed since it was read; read it again", ErrConflict)

// InvalidError is a validation failure.
type InvalidError = catalog.InvalidError

func invalid(format string, a ...any) error { return &InvalidError{Msg: fmt.Sprintf(format, a...)} }

// Keys seals and opens secret values; *secretstore.Keyring is one.
type Keys = catalog.Keys

// Table and column of the credential, as bound into the AAD.
const (
	Table            = "remote_agents"
	CredentialColumn = "credential"
)

// CredentialBinding is the AAD binding of a remote agent's credential.
func CredentialBinding(org, id pgtype.UUID) secretstore.Binding {
	return secretstore.Binding{OrgID: org.String(), Table: Table, RowID: id.String(), Column: CredentialColumn}
}

// Health values (the type is the one of MCP servers).
const (
	HealthUnknown   = "unknown"
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
)

// FailureThreshold is how many failed checks in a row make an agent unhealthy.
const FailureThreshold = mcpservers.FailureThreshold

// Reasons of the health check. They are fixed texts: a reason never carries data from the agent.
const (
	ReasonCardChanged   = "the agent card changed since it was pinned; an admin must read it and accept it"
	ReasonCardUnaccept  = "an agent card that differs from the pin was seen and is not accepted"
	ReasonUnreachable   = "the agent could not be reached"
	ReasonEgressDenied  = "the platform URL policy denies the agent's address"
	ReasonCardInvalid   = "the agent card could not be read as an agent card"
	ReasonCardHost      = "the agent card offers no interface on the host of the agent"
	ReasonCredentialBad = "the credential could not be opened"
)

// RemoteAgent is a remote agent as callers see it. It holds no credential and no card text.
type RemoteAgent struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	CardURL             string     `json:"cardUrl"`
	HasCredential       bool       `json:"hasCredential"`
	CredentialHeader    string     `json:"credentialHeader,omitempty"`
	CredentialPrefix    string     `json:"credentialPrefix,omitempty"`
	CredentialLostAt    *time.Time `json:"credentialLostAt,omitempty"`
	CABundle            string     `json:"caBundle,omitempty"`
	InsecureSkipVerify  bool       `json:"insecureSkipVerify,omitempty"`
	CardHash            string     `json:"cardHash"`
	CardChecked         *time.Time `json:"cardCheckedAt,omitempty"`
	ObservedCardHash    string     `json:"observedCardHash,omitempty"`
	Health              string     `json:"health"`
	HealthReason        string     `json:"healthReason,omitempty"`
	HealthCheckedAt     *time.Time `json:"healthCheckedAt,omitempty"`
	HealthSince         *time.Time `json:"healthSince,omitempty"`
	ConsecutiveFailures int        `json:"consecutiveFailures,omitempty"`
	DisabledReason      string     `json:"disabledReason,omitempty"`
	CreatedAt           time.Time  `json:"createdAt"`
	UpdatedAt           time.Time  `json:"updatedAt"`
}

// CardChanged says the health check saw a card that is not the pin and an admin did not accept it.
func (r RemoteAgent) CardChanged() bool { return r.ObservedCardHash != "" }

// Card is the pinned card and the observed one. The text is data from another party, for admins only.
type Card struct {
	Hash         string
	JSON         string
	ObservedHash string
	ObservedJSON string
}

// Service is the remote agent service layer.
type Service struct {
	pool        *pgxpool.Pool
	keys        Keys
	log         *slog.Logger
	policy      catalog.URLPolicy
	fetch       CardFetcher
	hold        *restorehold.Guard
	noTargetTLS bool
}

// New returns a Service. keys may be nil; agents with a credential then cannot be written.
func New(pool *pgxpool.Pool, keys Keys, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	s := &Service{pool: pool, keys: keys, log: log}
	s.fetch = s.defaultFetch
	return s
}

// WithURLPolicy sets which hosts remote agents may name. The same policy applies at registration and at
// each call and check (ADR 0120).
func (s *Service) WithURLPolicy(p catalog.URLPolicy) *Service {
	s.policy = p
	return s
}

// WithFetcher replaces the card fetcher. It is for tests.
func (s *Service) WithFetcher(f CardFetcher) *Service {
	s.fetch = f
	return s
}

// WithRestoreHold makes the outward calls of this service (a card read) refuse with restorehold.ErrHeld
// while g holds (ADR 0107).
func (s *Service) WithRestoreHold(g *restorehold.Guard) *Service {
	s.hold = g
	return s
}

// WithoutTargetTLS says the configured gateway cannot render a per-target TLS policy: a CA bundle or a
// skipped certificate check is then refused, as for MCP servers.
func (s *Service) WithoutTargetTLS() *Service {
	s.noTargetTLS = true
	return s
}

func (s *Service) checkTargetTLS(caBundle string, insecure bool) error {
	if s.noTargetTLS && (caBundle != "" || insecure) {
		return invalid("this installation's gateway (gateway.mode=kubernetes) cannot trust an agent's own CA or skip the certificate check, so caBundle and insecureSkipVerify are refused; use gateway.mode=standalone, or an agent with a certificate from a public CA")
	}
	return nil
}

func (s *Service) tx(ctx context.Context, fn func(q *store.Queries, b audit.Batch) error) error {
	_, err := audit.InTx(ctx, s.pool, fn)
	return err
}

func mapErr(err error) error {
	var pg *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case errors.As(err, &pg) && pg.Code == "23505":
		return ErrConflict
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

func toRemoteAgent(r store.RemoteAgent) RemoteAgent {
	return RemoteAgent{
		ID: r.ID.String(), Name: r.Name, CardURL: r.CardUrl,
		HasCredential: len(r.Credential) > 0, CredentialHeader: r.CredentialHeader.String, CredentialPrefix: r.CredentialPrefix,
		CredentialLostAt: timePtr(r.CredentialLostAt), CABundle: r.CaBundle, InsecureSkipVerify: r.InsecureSkipVerify,
		CardHash: r.CardHash, CardChecked: timePtr(r.CardCheckedAt), ObservedCardHash: r.ObservedCardHash.String,
		Health: string(r.Health), HealthReason: r.HealthReason.String, HealthCheckedAt: timePtr(r.HealthCheckedAt),
		HealthSince: timePtr(r.HealthSince), ConsecutiveFailures: int(r.ConsecutiveFailures), DisabledReason: r.DisabledReason.String,
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
}

// summary is what an audit event records of a remote agent: no credential and no card text.
func summary(r store.RemoteAgent) map[string]any {
	return map[string]any{
		"name": r.Name, "cardUrl": r.CardUrl, "hasCredential": len(r.Credential) > 0, "credentialHeader": r.CredentialHeader.String,
		"hasCABundle": r.CaBundle != "", "insecureSkipVerify": r.InsecureSkipVerify, "cardHash": r.CardHash,
	}
}

// ValidateName checks an agent's name. It is the rule of MCP server names: the model passes it as an
// argument of the built-in tools.
func ValidateName(name string) error { return mcpservers.ValidateName(name) }

// ValidateCardURL checks the base URL of an agent and the credential header and prefix sent to it.
func ValidateCardURL(rawURL, header, prefix string) error {
	u, err := url.Parse(rawURL)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return invalid("cardUrl must be the http(s) base URL of an A2A agent, for example https://agent.example.com")
	case u.User != nil:
		return invalid("cardUrl must not carry credentials; put them in the credential")
	case u.RawQuery != "" || u.Fragment != "":
		return invalid("cardUrl must not have a query or fragment, which could carry a secret; use a credential header")
	case len(rawURL) > 2048:
		return invalid("cardUrl is longer than 2048 characters")
	case strings.Contains(rawURL, "$"):
		return invalid("cardUrl must not contain '$': the gateway's config loader reads $NAME as an environment variable")
	}
	return mcpservers.ValidateCredentialFields(rawURL, header, prefix, "remote agent")
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
		return nil, fmt.Errorf("remoteagents: seal credential: %w", err)
	}
	return json.Marshal(env)
}

// OpenCredential decrypts the credential of a remote agent, or returns "" for one without a credential.
func OpenCredential(keys catalog.Opener, org, id pgtype.UUID, envelope []byte) (string, error) {
	if len(envelope) == 0 {
		return "", nil
	}
	if keys == nil {
		return "", ErrNoKeys
	}
	var env secretstore.Envelope
	if err := json.Unmarshal(envelope, &env); err != nil {
		return "", invalid("remote agent credential envelope is unreadable")
	}
	plain, err := keys.Open(CredentialBinding(org, id), env)
	if err != nil {
		return "", err
	}
	defer clear(plain)
	return string(plain), nil
}

// Input registers a remote agent.
type Input struct {
	Name    string
	CardURL string
	// CredentialValue is the secret; empty means the agent needs none. CredentialHeader defaults to
	// Authorization. CredentialPrefix is what precedes the secret in the header ("Bearer ").
	CredentialHeader   string
	CredentialPrefix   string
	CredentialValue    string
	CABundle           string
	InsecureSkipVerify bool
}

// Patch changes a remote agent; nil fields stay.
type Patch struct {
	// CardURL moves the agent to another URL. The pin then moves to the card read at the new URL: a
	// different URL is a different agent.
	CardURL *string
	// Credential replaces the credential; RemoveCredential removes it. Setting both is invalid.
	Credential         *CredentialPatch
	RemoveCredential   bool
	CABundle           *string
	InsecureSkipVerify *bool
}

// CredentialPatch replaces the header, prefix or secret. Value nil keeps the stored secret.
type CredentialPatch struct {
	Header string
	Prefix string
	Value  *string
}

func prefixIf(prefix string, has bool) string {
	if !has {
		return ""
	}
	return prefix
}

// checkCABudget refuses a CA bundle that takes the org's bundles (MCP servers and remote agents together,
// as the reconciler serves them from one Secret) over mcpservers.MaxOrgCABundleBytes. except is the agent
// being saved (zero for a new one).
func checkCABudget(ctx context.Context, q *store.Queries, org, except pgtype.UUID, bundle string) error {
	if bundle == "" {
		return nil
	}
	if err := q.LockMCPCABudget(ctx); err != nil {
		return err
	}
	mcp, err := q.ListMCPServers(ctx, org)
	if err != nil {
		return err
	}
	total := len(bundle)
	for _, r := range mcp {
		if !r.RemovedAt.Valid {
			total += len(r.CaBundle)
		}
	}
	agents, err := q.SumRemoteAgentCABundleBytes(ctx, store.SumRemoteAgentCABundleBytesParams{OrgID: org, ExceptID: except})
	if err != nil {
		return err
	}
	if int64(total)+agents > mcpservers.MaxOrgCABundleBytes {
		return invalid("the CA bundles of this org's MCP servers and remote agents are over %d KiB in total", mcpservers.MaxOrgCABundleBytes>>10)
	}
	return nil
}

// List returns the org's remote agents.
func (s *Service) List(ctx context.Context, org pgtype.UUID) ([]RemoteAgent, error) {
	rows, err := store.New(s.pool).ListRemoteAgents(ctx, org)
	if err != nil {
		return nil, err
	}
	out := make([]RemoteAgent, 0, len(rows))
	for _, r := range rows {
		out = append(out, toRemoteAgent(r))
	}
	return out, nil
}

// Get returns one remote agent.
func (s *Service) Get(ctx context.Context, org pgtype.UUID, id string) (RemoteAgent, error) {
	rid, err := parseID(id)
	if err != nil {
		return RemoteAgent{}, err
	}
	row, err := store.New(s.pool).GetRemoteAgent(ctx, store.GetRemoteAgentParams{OrgID: org, ID: rid})
	if err != nil {
		return RemoteAgent{}, mapErr(err)
	}
	return toRemoteAgent(row), nil
}

// Create registers a remote agent. It reads the agent card at the URL and pins it: an agent whose card
// cannot be read, or offers no interface on its own host, is refused.
func (s *Service) Create(ctx context.Context, org pgtype.UUID, actor audit.Actor, creator pgtype.UUID, in Input) (RemoteAgent, error) {
	if in.CredentialValue != "" && in.CredentialHeader == "" {
		in.CredentialHeader = "Authorization"
	}
	if err := ValidateName(in.Name); err != nil {
		return RemoteAgent{}, err
	}
	if err := ValidateCardURL(in.CardURL, in.CredentialHeader, in.CredentialPrefix); err != nil {
		return RemoteAgent{}, err
	}
	if in.CredentialValue == "" && in.CredentialHeader != "" {
		return RemoteAgent{}, invalid("a credential needs a value")
	}
	if err := mcpservers.ValidateTLSTrust(in.CardURL, in.CABundle, in.InsecureSkipVerify); err != nil {
		return RemoteAgent{}, err
	}
	if err := s.checkTargetTLS(in.CABundle, in.InsecureSkipVerify); err != nil {
		return RemoteAgent{}, err
	}
	if err := s.policy.Check(ctx, in.CardURL); err != nil {
		return RemoteAgent{}, err
	}
	card, err := s.read(ctx, FetchRequest{
		URL: in.CardURL, Header: in.CredentialHeader, Prefix: in.CredentialPrefix, Secret: in.CredentialValue,
		CABundle: in.CABundle, InsecureSkipVerify: in.InsecureSkipVerify,
	})
	if err != nil {
		return RemoteAgent{}, err
	}
	id, err := newID()
	if err != nil {
		return RemoteAgent{}, err
	}
	cred, err := s.seal(org, id, in.CredentialValue)
	if err != nil {
		return RemoteAgent{}, err
	}
	var row store.RemoteAgent
	err = s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		if err := checkCABudget(ctx, q, org, pgtype.UUID{}, in.CABundle); err != nil {
			return err
		}
		row, err = q.CreateRemoteAgent(ctx, store.CreateRemoteAgentParams{
			OrgID: org, ID: id, Name: in.Name, CardUrl: in.CardURL,
			CredentialHeader: pgtype.Text{String: in.CredentialHeader, Valid: len(cred) > 0}, CredentialPrefix: prefixIf(in.CredentialPrefix, len(cred) > 0),
			Credential: cred, CaBundle: in.CABundle, InsecureSkipVerify: in.InsecureSkipVerify,
			CardHash: card.Hash, Card: card.JSON, CreatedBy: creator,
		})
		if err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "remote_agent.created", TargetKind: "remote_agent", TargetID: id.String(), After: summary(row),
		})
		return nil
	})
	if err != nil {
		return RemoteAgent{}, mapErr(err)
	}
	return toRemoteAgent(row), nil
}

// Update changes a remote agent. A change of the URL reads the card at the new URL and moves the pin to it.
// A change of the credential or of the TLS trust keeps the pin and starts health over.
func (s *Service) Update(ctx context.Context, org pgtype.UUID, actor audit.Actor, id string, patch Patch) (RemoteAgent, error) {
	rid, err := parseID(id)
	if err != nil {
		return RemoteAgent{}, err
	}
	if patch.Credential != nil && patch.RemoveCredential {
		return RemoteAgent{}, invalid("set a credential or remove it, not both")
	}
	var out store.RemoteAgent
	err = s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		cur, err := q.GetRemoteAgentForUpdate(ctx, store.GetRemoteAgentForUpdateParams{OrgID: org, ID: rid})
		if err != nil {
			return err
		}
		u := cur.CardUrl
		if patch.CardURL != nil {
			u = *patch.CardURL
		}
		caBundle := cur.CaBundle
		if patch.CABundle != nil {
			caBundle = *patch.CABundle
		}
		insecure := cur.InsecureSkipVerify
		if patch.InsecureSkipVerify != nil {
			insecure = *patch.InsecureSkipVerify
		}
		header, prefix, cred := cur.CredentialHeader.String, cur.CredentialPrefix, cur.Credential
		value, haveValue := "", false
		switch {
		case patch.RemoveCredential:
			header, prefix, cred = "", "", nil
		case patch.Credential != nil:
			header, prefix = patch.Credential.Header, patch.Credential.Prefix
			if header == "" {
				header = "Authorization"
			}
			if patch.Credential.Value != nil {
				value, haveValue = *patch.Credential.Value, true
				if cred, err = s.seal(org, rid, value); err != nil {
					return err
				}
			}
			if len(cred) == 0 {
				return invalid("a credential needs a value")
			}
		}
		if err := ValidateCardURL(u, header, prefix); err != nil {
			return err
		}
		if err := mcpservers.ValidateTLSTrust(u, caBundle, insecure); err != nil {
			return err
		}
		if err := s.checkTargetTLS(caBundle, insecure); err != nil {
			return err
		}
		if err := s.policy.Check(ctx, u); err != nil {
			return err
		}
		p := store.UpdateRemoteAgentParams{
			OrgID: org, ID: rid, CardUrl: u, CaBundle: caBundle, InsecureSkipVerify: insecure,
			CredentialHeader: pgtype.Text{String: header, Valid: len(cred) > 0}, CredentialPrefix: prefixIf(prefix, len(cred) > 0),
			// The envelope is rewritten only when this patch replaces or removes it: `keys rewrap` may have
			// replaced the stored one since cur was read (see UpdateMCPServer).
			SetCredentialMeta: patch.Credential != nil || patch.RemoveCredential,
			SetCredential:     patch.RemoveCredential || haveValue, Credential: cred,
		}
		if u != cur.CardUrl {
			// A different URL is a different agent. The card read at the new URL becomes the pin. The
			// credential sent there is the one this patch sets, or the stored one when it stays.
			secret := value
			if !haveValue && len(cred) > 0 {
				if secret, err = OpenCredential(s.keys, org, rid, cred); err != nil {
					return err
				}
			}
			card, err := s.read(ctx, FetchRequest{
				URL: u, Header: header, Prefix: prefix, Secret: secret, CABundle: caBundle, InsecureSkipVerify: insecure,
			})
			if err != nil {
				return err
			}
			p.SetCard, p.CardHash, p.Card = true, card.Hash, card.JSON
		}
		if caBundle != "" && caBundle != cur.CaBundle {
			if err := checkCABudget(ctx, q, org, rid, caBundle); err != nil {
				return err
			}
		}
		out, err = q.UpdateRemoteAgent(ctx, p)
		if err != nil {
			return err
		}
		after := summary(out)
		after["credentialChanged"] = patch.Credential != nil || patch.RemoveCredential
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "remote_agent.updated", TargetKind: "remote_agent", TargetID: id,
			Before: summary(cur), After: after,
		})
		return nil
	})
	if err != nil {
		return RemoteAgent{}, mapErr(err)
	}
	return toRemoteAgent(out), nil
}

// Delete removes a remote agent. Its grants go with it.
func (s *Service) Delete(ctx context.Context, org pgtype.UUID, actor audit.Actor, id string) error {
	rid, err := parseID(id)
	if err != nil {
		return err
	}
	err = s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		cur, err := q.GetRemoteAgent(ctx, store.GetRemoteAgentParams{OrgID: org, ID: rid})
		if err != nil {
			return err
		}
		if _, err := q.DeleteRemoteAgent(ctx, store.DeleteRemoteAgentParams{OrgID: org, ID: rid}); err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "remote_agent.deleted", TargetKind: "remote_agent", TargetID: id, Before: summary(cur),
		})
		return nil
	})
	return mapErr(err)
}

// GetCard returns the pinned card and the observed one. The text is data from another party: the caller
// shows it to an admin and nobody else.
func (s *Service) GetCard(ctx context.Context, org pgtype.UUID, id string) (Card, error) {
	rid, err := parseID(id)
	if err != nil {
		return Card{}, err
	}
	row, err := store.New(s.pool).GetRemoteAgent(ctx, store.GetRemoteAgentParams{OrgID: org, ID: rid})
	if err != nil {
		return Card{}, mapErr(err)
	}
	return Card{Hash: row.CardHash, JSON: row.Card, ObservedHash: row.ObservedCardHash.String, ObservedJSON: row.ObservedCard.String}, nil
}

// AcceptCard moves the observed card to the pin. observedHash is the hash the admin read; if the health
// check saw another card since, the call is refused with ErrCardMoved, so an admin never accepts a card
// they did not read. It clears the finding and starts health over.
func (s *Service) AcceptCard(ctx context.Context, org pgtype.UUID, actor audit.Actor, id, observedHash string) (RemoteAgent, error) {
	rid, err := parseID(id)
	if err != nil {
		return RemoteAgent{}, err
	}
	var out store.RemoteAgent
	err = s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		cur, err := q.GetRemoteAgentForUpdate(ctx, store.GetRemoteAgentForUpdateParams{OrgID: org, ID: rid})
		if err != nil {
			return err
		}
		if !cur.ObservedCardHash.Valid {
			return invalid("there is no observed card to accept")
		}
		if cur.ObservedCardHash.String != observedHash {
			return ErrCardMoved
		}
		out, err = q.AcceptRemoteAgentCard(ctx, store.AcceptRemoteAgentCardParams{OrgID: org, ID: rid})
		if err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "remote_agent.card.accepted", TargetKind: "remote_agent", TargetID: id,
			Before: map[string]any{"name": cur.Name, "cardHash": cur.CardHash},
			After:  map[string]any{"name": out.Name, "cardHash": out.CardHash},
		})
		return nil
	})
	if err != nil {
		return RemoteAgent{}, mapErr(err)
	}
	return toRemoteAgent(out), nil
}

// read fetches a card, refusing while a restore holds. The error is mapped to a message that is safe to show.
func (s *Service) read(ctx context.Context, r FetchRequest) (*a2aout.Card, error) {
	if err := s.hold.Err(); err != nil {
		return nil, err
	}
	card, err := s.fetch(ctx, r, s.policy)
	if err != nil {
		return nil, invalid("the agent card could not be read: %s", reasonOf(err))
	}
	return card, nil
}
