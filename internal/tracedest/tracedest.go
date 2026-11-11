// SPDX-License-Identifier: Apache-2.0

// Package tracedest is the service layer of run-trace destinations (ADR 0074): an org's OTLP
// destinations, its content level and sampling, the platform destination and ceiling, and the
// decision whether a run is traced. The connect-rpc service and, later, the trace router use it.
//
// Creating or changing a destination first sends it one synthetic trace and saves only when the
// backend accepts it (decision 9). Credentials are sealed in the secret store the way provider keys
// are (ADR 0056) and are never returned, audited or logged.
package tracedest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/outward"
	"github.com/stateracloud/koinon/internal/restorehold"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/tracing/export"
)

// Errors. InvalidError and the not-found and conflict errors are the catalog's, so the service layers
// that map them (services.catalogError, adminapi) treat these the same way.
var (
	ErrNotFound = catalog.ErrNotFound
	ErrConflict = catalog.ErrConflict
	ErrNoKeys   = errors.New("no key-encryption keys are configured, so trace credentials cannot be stored")
	// ErrChanged means the destination was changed or deleted since the caller read it.
	ErrChanged = errors.New("the destination changed since you loaded it; reload and try again")
	// ErrOwnerOnly means the change needs an org owner (content level full).
	ErrOwnerOnly = errors.New("only an org owner may set the content level full")
	// ErrOwnerAtFull means the org's level is full, so adding a destination or changing its connection
	// needs an owner.
	ErrOwnerAtFull = errors.New("the org's content level is full, so only an owner may add a destination or change where one sends")
	// ErrNotMember means the caller is not a member of the org (a platform admin acting from outside)
	// and may lower its settings but not raise them.
	ErrNotMember = errors.New("only a member of the org can raise its trace content level or the platform copy")
	// ErrAboveCeiling means the level is above what the platform allows.
	ErrAboveCeiling = errors.New("the content level is above the platform ceiling")
	// ErrCredentialLost means the stored credentials were lost with a key (ADR 0085 decision 13):
	// there is nothing to test until they are entered again.
	ErrCredentialLost = &InvalidError{Msg: "the destination's credentials were lost; enter them again and save"}
)

// InvalidError is a validation failure.
type InvalidError = catalog.InvalidError

func invalid(format string, a ...any) error { return &InvalidError{Msg: fmt.Sprintf(format, a...)} }

// TestFailedError means the backend did not take the test trace, so nothing was saved.
type TestFailedError struct{ Outcome export.Outcome }

func (e *TestFailedError) Error() string {
	return "the destination was not saved: the test trace failed: " + e.Outcome.Message
}

// Keys seals and opens secret values; *secretstore.Keyring is one.
type Keys = catalog.Keys

// Content levels, in order: a higher level carries more.
const (
	LevelNone     = "none"
	LevelRedacted = "redacted"
	LevelFull     = "full"
)

var levels = []string{LevelNone, LevelRedacted, LevelFull}

// ValidLevel reports whether s is a content level.
func ValidLevel(s string) bool { return slices.Contains(levels, s) }

// LevelRank orders the levels: none 0, redacted 1, full 2; an unknown level ranks -1.
func LevelRank(s string) int { return slices.Index(levels, s) }

// MinLevel is the lower of two levels.
func MinLevel(a, b string) string {
	if LevelRank(a) <= LevelRank(b) {
		return a
	}
	return b
}

// Table and column names of the sealed credentials, as bound into the AAD.
const (
	DestinationsTable = "trace_destinations"
	PlatformTable     = "platform_trace_destination"
	CredentialsColumn = "credentials"
	platformOrg       = "platform"
	platformRow       = "platform"
)

// Binding is the AAD binding of an org destination's credentials.
func Binding(org, id pgtype.UUID) secretstore.Binding {
	return secretstore.Binding{OrgID: org.String(), Table: DestinationsTable, RowID: id.String(), Column: CredentialsColumn}
}

// PlatformBinding is the AAD binding of the platform destination's credentials.
func PlatformBinding() secretstore.Binding {
	return secretstore.Binding{OrgID: platformOrg, Table: PlatformTable, RowID: platformRow, Column: CredentialsColumn}
}

// Service manages destinations and settings.
type Service struct {
	pool   *pgxpool.Pool
	keys   Keys
	policy catalog.URLPolicy
	client *export.Client
	log    *slog.Logger
	now    func() time.Time
	hold   *restorehold.Guard
}

// WithRestoreHold makes every method that sends a trace (Create, Update, Test and the platform
// ones) refuse with restorehold.ErrHeld while g holds (issue #462, ADR 0107).
func (s *Service) WithRestoreHold(g *restorehold.Guard) *Service {
	s.hold = g
	return s
}

// New returns a Service. keys may be nil: destinations without secrets still work. The client dials
// through policy, so a destination can only reach what the egress policy allows.
func New(pool *pgxpool.Pool, keys Keys, policy catalog.URLPolicy, log *slog.Logger) (*Service, error) {
	client, err := export.NewClient(policy.DialContext(&net.Dialer{Timeout: 5 * time.Second}), 0)
	if err != nil {
		return nil, err
	}
	return NewWithClient(pool, keys, policy, client, log), nil
}

// NewWithClient is New with the client the tests (or the router, to share connections) supply.
func NewWithClient(pool *pgxpool.Pool, keys Keys, policy catalog.URLPolicy, client *export.Client, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: pool, keys: keys, policy: policy, client: client, log: log, now: time.Now}
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

// TestResult is the last synthetic trace of a destination.
type TestResult struct {
	OK       bool
	Message  string
	TestedAt time.Time
}

// Health is what the router recorded.
type Health struct {
	LastSuccessAt *time.Time
	LastErrorAt   *time.Time
	LastError     string
	SpansDropped  int64
}

// Destination is a destination as stored. It never holds a credential.
type Destination struct {
	// ID is empty for the platform destination.
	ID                 string
	Name               string
	Backend            string
	EndpointURL        string
	Params             map[string]string
	SecretFields       []string
	ResourceAttributes map[string]string
	Enabled            bool
	LastTest           *TestResult
	Health             Health
	// CredentialLostAt is set when the stored credentials were lost with a key (ADR 0085 decision
	// 13): nothing is exported to the destination until they are entered again.
	CredentialLostAt *time.Time
	// AdminDisabled is set while an admin holds the destination off (the outward switch, ADR 0110 decision 5).
	AdminDisabled *outward.Disabled
	// ConfigVersion changes with every change of the configuration and with nothing the router records.
	ConfigVersion int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Authority is what the caller is in the org, from its real membership: a platform admin who is not
// a member holds neither. Owner may set the content level full and add or re-point a destination
// while the level is full; Member may raise the level and the platform copy.
type Authority struct{ Owner, Member bool }

// Input is what a caller writes. For an update, Secrets are merged into the stored ones by name and
// RemoveSecrets clear names; changing Backend starts from no secrets.
type Input struct {
	Name               string
	Backend            string
	EndpointURL        string
	Params             map[string]string
	Secrets            map[string]string
	RemoveSecrets      []string
	ResourceAttributes map[string]string
	Enabled            bool
}

var (
	namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)
	attrKey     = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
)

const maxResourceAttributes = 32

func validateAttributes(attrs map[string]string) error {
	if len(attrs) > maxResourceAttributes {
		return invalid("at most %d resource attributes are allowed", maxResourceAttributes)
	}
	for k, v := range attrs {
		switch {
		case !attrKey.MatchString(k):
			return invalid("resource attribute %q: a name is lower-case letters, digits, '.', '_' and '-'", k)
		case strings.HasPrefix(k, "koinon.") || k == "service.name" || strings.HasPrefix(k, "gen_ai."):
			return invalid("resource attribute %q is set by Koinon", k)
		case len(v) > 256:
			return invalid("resource attribute %q is longer than 256 characters", k)
		}
	}
	return nil
}

// prepared is an input checked and merged with what is stored, ready to test and save.
type prepared struct {
	endpoint string
	dest     export.Destination
	envelope []byte
	fields   []string
	params   []byte
	attrs    []byte
	in       Input
}

// prepare validates in, merges its secrets into existing (the opened stored secrets, nil for a new
// destination or a changed backend), seals the result for binding b and builds what the client sends.
func (s *Service) prepare(ctx context.Context, in Input, existing map[string]string, b secretstore.Binding) (prepared, error) {
	preset, err := export.ParsePreset(in.Backend)
	if err != nil {
		return prepared{}, invalid("%s", err.Error())
	}
	if err := validateAttributes(in.ResourceAttributes); err != nil {
		return prepared{}, err
	}
	secrets := map[string]string{}
	for k, v := range existing {
		secrets[k] = v
	}
	for _, k := range in.RemoveSecrets {
		delete(secrets, k)
	}
	for k, v := range in.Secrets {
		if v == "" {
			return prepared{}, invalid("secret %q is empty; to clear it, list it under remove_secrets", k)
		}
		secrets[k] = v
	}
	endpoint, err := s.endpoint(in.EndpointURL)
	if err != nil {
		return prepared{}, err
	}
	d := export.Destination{Preset: preset, Endpoint: endpoint, Params: nonNil(in.Params), Secrets: secrets}
	if err := d.Validate(); err != nil {
		var ie *export.InvalidError
		if errors.As(err, &ie) {
			return prepared{}, invalid("%s", ie.Msg)
		}
		return prepared{}, err
	}
	if err := s.policy.Check(ctx, endpoint); err != nil {
		return prepared{}, err
	}
	p := prepared{endpoint: endpoint, dest: d, in: in}
	p.fields = make([]string, 0, len(secrets))
	for k := range secrets {
		p.fields = append(p.fields, k)
	}
	slices.Sort(p.fields)
	if len(secrets) > 0 {
		if s.keys == nil {
			return prepared{}, ErrNoKeys
		}
		plain, err := json.Marshal(secrets)
		if err != nil {
			return prepared{}, err
		}
		env, err := s.keys.Seal(b, plain)
		clear(plain)
		if err != nil {
			return prepared{}, fmt.Errorf("tracedest: seal credentials: %w", err)
		}
		if p.envelope, err = json.Marshal(env); err != nil {
			return prepared{}, err
		}
	}
	if p.params, err = json.Marshal(nonNil(in.Params)); err != nil {
		return prepared{}, err
	}
	if p.attrs, err = json.Marshal(nonNil(in.ResourceAttributes)); err != nil {
		return prepared{}, err
	}
	return p, nil
}

// endpoint normalises the endpoint and requires https, except for a host the platform lists in its
// allowed hosts (a lab or test backend).
func (s *Service) endpoint(raw string) (string, error) {
	norm, err := export.NormalizeEndpoint(raw)
	if err != nil {
		var ie *export.InvalidError
		if errors.As(err, &ie) {
			return "", invalid("%s", ie.Msg)
		}
		return "", err
	}
	u, _ := url.Parse(norm)
	if u.Scheme == "https" || s.allowedPlain(u) {
		return norm, nil
	}
	return "", invalid("endpoint must be an https URL; plain http is allowed only for a host in the platform's allowed hosts")
}

func (s *Service) allowedPlain(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	for _, a := range s.policy.AllowedHosts {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == host || a == strings.ToLower(u.Host) {
			return true
		}
		if pfx, err := netip.ParsePrefix(a); err == nil {
			if ip, err := netip.ParseAddr(host); err == nil && pfx.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// connectionTouched says the change alters where or how the destination is reached: backend,
// endpoint, settings or secrets. Only such a change is tested before saving.
func connectionTouched(cur store.TraceDestination, endpoint string, in Input) bool {
	return string(cur.Backend) != in.Backend || cur.EndpointUrl != endpoint ||
		!maps.Equal(decodeMap(cur.Params), nonNil(in.Params)) || len(in.Secrets) > 0 || len(in.RemoveSecrets) > 0
}

// requireOwnerAtFull refuses a non-owner who adds or re-points a destination while the org's content
// level is full: the destination would receive full content the caller may not choose to send.
func (s *Service) requireOwnerAtFull(ctx context.Context, q *store.Queries, org pgtype.UUID, who Authority) error {
	if who.Owner {
		return nil
	}
	st, err := s.settings(ctx, q, org)
	if err != nil {
		return err
	}
	if MinLevel(st.ContentLevel, st.Ceiling) == LevelFull {
		return ErrOwnerAtFull
	}
	return nil
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// test sends the synthetic trace and returns the outcome and the row columns that record it.
func (s *Service) test(ctx context.Context, d export.Destination, org string, attrs map[string]string) (export.Outcome, time.Time) {
	now := s.now()
	return s.client.Test(ctx, d, org, attrs, now), now
}

// open decrypts a destination's credentials with binding b; a destination without any gives nil.
func (s *Service) open(envelope []byte, b secretstore.Binding) (map[string]string, error) {
	if len(envelope) == 0 {
		return nil, nil
	}
	if s.keys == nil {
		return nil, ErrNoKeys
	}
	var env secretstore.Envelope
	if err := json.Unmarshal(envelope, &env); err != nil {
		return nil, invalid("the stored credentials are unreadable")
	}
	plain, err := s.keys.Open(b, env)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	out := map[string]string{}
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, invalid("the stored credentials are unreadable")
	}
	return out, nil
}

func decodeMap(raw []byte) map[string]string {
	out := map[string]string{}
	_ = json.Unmarshal(raw, &out)
	return out
}

func optTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

func toTest(at pgtype.Timestamptz, ok pgtype.Bool, msg pgtype.Text) *TestResult {
	if !at.Valid || !ok.Valid {
		return nil
	}
	return &TestResult{OK: ok.Bool, Message: msg.String, TestedAt: at.Time}
}

func toDestination(d store.TraceDestination) Destination {
	return Destination{
		ID: d.ID.String(), Name: d.Name, Backend: string(d.Backend), EndpointURL: d.EndpointUrl,
		Params: decodeMap(d.Params), SecretFields: d.SecretFields, ResourceAttributes: decodeMap(d.ResourceAttributes),
		Enabled: d.Enabled, LastTest: toTest(d.LastTestAt, d.LastTestOk, d.LastTestMessage),
		Health:        Health{LastSuccessAt: optTime(d.LastSuccessAt), LastErrorAt: optTime(d.LastErrorAt), LastError: d.LastError.String, SpansDropped: d.SpansDropped},
		ConfigVersion: d.ConfigVersion, CreatedAt: d.CreatedAt.Time, UpdatedAt: d.UpdatedAt.Time,
		CredentialLostAt: optTime(d.CredentialLostAt),
		AdminDisabled:    outward.FromRow(d.AdminDisabledAt, d.AdminDisabledBy, d.AdminDisabledNote),
	}
}

func toPlatform(d store.PlatformTraceDestination) Destination {
	return Destination{
		Backend: string(d.Backend), EndpointURL: d.EndpointUrl, Params: decodeMap(d.Params), SecretFields: d.SecretFields,
		ResourceAttributes: decodeMap(d.ResourceAttributes), Enabled: d.Enabled,
		LastTest:      toTest(d.LastTestAt, d.LastTestOk, d.LastTestMessage),
		Health:        Health{LastSuccessAt: optTime(d.LastSuccessAt), LastErrorAt: optTime(d.LastErrorAt), LastError: d.LastError.String, SpansDropped: d.SpansDropped},
		ConfigVersion: d.ConfigVersion, UpdatedAt: d.UpdatedAt.Time,
		CredentialLostAt: optTime(d.CredentialLostAt),
		AdminDisabled:    outward.FromRow(d.AdminDisabledAt, d.AdminDisabledBy, d.AdminDisabledNote),
	}
}

// auditView describes a destination for the audit log: no secret value, and the endpoint without its
// query, which may carry a token.
func auditView(d Destination) map[string]any {
	endpoint := d.EndpointURL
	if u, err := url.Parse(endpoint); err == nil {
		endpoint = u.Scheme + "://" + u.Host + u.EscapedPath()
	}
	fields := d.SecretFields
	if fields == nil {
		fields = []string{}
	}
	return map[string]any{
		"name": d.Name, "backend": d.Backend, "endpoint": endpoint, "enabled": d.Enabled,
		"secretFields": fields, "paramNames": paramNames(d.Params), "resourceAttributeNames": paramNames(d.ResourceAttributes),
	}
}

func paramNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// List returns the org's destinations by name.
func (s *Service) List(ctx context.Context, org pgtype.UUID) ([]Destination, error) {
	rows, err := store.New(s.pool).ListTraceDestinations(ctx, org)
	if err != nil {
		return nil, err
	}
	out := make([]Destination, 0, len(rows))
	for _, r := range rows {
		out = append(out, toDestination(r))
	}
	return out, nil
}

// Get returns one destination.
func (s *Service) Get(ctx context.Context, org pgtype.UUID, id string) (Destination, error) {
	row, err := s.row(ctx, store.New(s.pool), org, id)
	if err != nil {
		return Destination{}, err
	}
	return toDestination(row), nil
}

func (s *Service) row(ctx context.Context, q *store.Queries, org pgtype.UUID, id string) (store.TraceDestination, error) {
	var u pgtype.UUID
	if err := u.Scan(id); err != nil || !u.Valid {
		return store.TraceDestination{}, ErrNotFound
	}
	row, err := q.GetTraceDestination(ctx, store.GetTraceDestinationParams{OrgID: org, ID: u})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.TraceDestination{}, ErrNotFound
	}
	return row, err
}

func testColumns(o export.Outcome, at time.Time) (pgtype.Timestamptz, pgtype.Bool, pgtype.Text) {
	return pgtype.Timestamptz{Time: at, Valid: true}, pgtype.Bool{Bool: o.OK, Valid: true}, pgtype.Text{String: o.Message, Valid: o.Message != ""}
}

// Create tests, then saves, a new destination. The test trace carries the org's ID. A backend that
// does not take it gives a *TestFailedError and nothing is saved. While the org's content level is
// full, only an owner may add a destination.
func (s *Service) Create(ctx context.Context, org pgtype.UUID, actor audit.Actor, user pgtype.UUID, who Authority, in Input) (Destination, error) {
	if err := s.hold.Err(); err != nil {
		return Destination{}, err
	}
	if !namePattern.MatchString(in.Name) {
		return Destination{}, invalid("name must be 1 to 64 characters of a-z, 0-9 and '-', starting and ending with a letter or digit")
	}
	if err := s.requireOwnerAtFull(ctx, store.New(s.pool), org, who); err != nil {
		return Destination{}, err
	}
	id := pgtype.UUID{Bytes: uuid.Must(uuid.NewV7()), Valid: true}
	p, err := s.prepare(ctx, in, nil, Binding(org, id))
	if err != nil {
		return Destination{}, err
	}
	outcome, at := s.test(ctx, p.dest, org.String(), in.ResourceAttributes)
	if !outcome.OK {
		return Destination{}, &TestFailedError{Outcome: outcome}
	}
	testAt, testOK, testMsg := testColumns(outcome, at)
	var out Destination
	err = s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		// The level may have been raised while the test was on its way.
		if err := s.requireOwnerAtFull(ctx, q, org, who); err != nil {
			return err
		}
		row, err := q.InsertTraceDestination(ctx, store.InsertTraceDestinationParams{
			OrgID: org, ID: id, Name: in.Name, Backend: store.TraceBackend(p.dest.Preset), EndpointUrl: p.endpoint,
			Params: p.params, Credentials: p.envelope, SecretFields: p.fields, ResourceAttributes: p.attrs, Enabled: in.Enabled,
			LastTestAt: testAt, LastTestOk: testOK, LastTestMessage: testMsg, CreatedBy: user,
		})
		if err != nil {
			return err
		}
		out = toDestination(row)
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "trace.destination.create", TargetKind: "trace_destination", TargetID: out.ID,
			After: auditView(out),
		})
		return nil
	})
	if isUnique(err) {
		return Destination{}, ErrConflict
	}
	return out, err
}

// Update saves the changes to a destination. Secrets in in are merged into the stored ones, but only
// while backend and endpoint origin stay the same: a destination re-pointed elsewhere needs every
// secret again, so stored credentials are never sent to an endpoint the admin just typed. A change to
// the connection (backend, endpoint, settings, secrets) is tested first; one that touches none of
// those (enabled, name, resource attributes) is saved untested, so a destination whose backend is
// down can still be disabled. expectedVersion, when set, must be the ConfigVersion the caller read.
func (s *Service) Update(ctx context.Context, org pgtype.UUID, actor audit.Actor, who Authority, id string, expectedVersion *int64, in Input) (Destination, error) {
	if err := s.hold.Err(); err != nil {
		return Destination{}, err
	}
	if !namePattern.MatchString(in.Name) {
		return Destination{}, invalid("name must be 1 to 64 characters of a-z, 0-9 and '-', starting and ending with a letter or digit")
	}
	cur, err := s.row(ctx, store.New(s.pool), org, id)
	if err != nil {
		return Destination{}, err
	}
	if expectedVersion != nil && *expectedVersion != cur.ConfigVersion {
		return Destination{}, ErrChanged
	}
	endpoint, err := s.endpoint(in.EndpointURL)
	if err != nil {
		return Destination{}, err
	}
	touched := connectionTouched(cur, endpoint, in)
	var p prepared
	var outcome export.Outcome
	var at time.Time
	if touched {
		if err := s.requireOwnerAtFull(ctx, store.New(s.pool), org, who); err != nil {
			return Destination{}, err
		}
		var existing map[string]string
		if string(cur.Backend) == in.Backend && export.SameOrigin(cur.EndpointUrl, endpoint) {
			if existing, err = s.open(cur.Credentials, Binding(org, cur.ID)); err != nil {
				return Destination{}, err
			}
		}
		if p, err = s.prepare(ctx, in, existing, Binding(org, cur.ID)); err != nil {
			return Destination{}, err
		}
		if outcome, at = s.test(ctx, p.dest, org.String(), in.ResourceAttributes); !outcome.OK {
			return Destination{}, &TestFailedError{Outcome: outcome}
		}
	} else {
		if err := validateAttributes(in.ResourceAttributes); err != nil {
			return Destination{}, err
		}
		p = prepared{endpoint: cur.EndpointUrl, envelope: cur.Credentials, fields: cur.SecretFields, params: cur.Params}
		p.dest.Preset = export.Preset(cur.Backend)
		if p.attrs, err = json.Marshal(nonNil(in.ResourceAttributes)); err != nil {
			return Destination{}, err
		}
	}
	testAt, testOK, testMsg := testColumns(outcome, at)
	before := toDestination(cur)
	var out Destination
	err = s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		row, err := q.UpdateTraceDestination(ctx, store.UpdateTraceDestinationParams{
			OrgID: org, ID: cur.ID, ExpectedVersion: cur.ConfigVersion, Name: in.Name, Backend: store.TraceBackend(p.dest.Preset),
			EndpointUrl: p.endpoint, Params: p.params, Credentials: p.envelope, SecretFields: p.fields,
			ResourceAttributes: p.attrs, Enabled: in.Enabled, SetTest: touched, LastTestAt: testAt, LastTestOk: testOK, LastTestMessage: testMsg,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrChanged
		}
		if err != nil {
			return err
		}
		out = toDestination(row)
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "trace.destination.update", TargetKind: "trace_destination", TargetID: out.ID,
			Before: auditView(before), After: auditView(out),
		})
		return nil
	})
	if isUnique(err) {
		return Destination{}, ErrConflict
	}
	return out, err
}

// Delete removes a destination.
func (s *Service) Delete(ctx context.Context, org pgtype.UUID, actor audit.Actor, id string) error {
	return s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		cur, err := s.row(ctx, q, org, id)
		if err != nil {
			return err
		}
		if _, err := q.DeleteTraceDestination(ctx, store.DeleteTraceDestinationParams{OrgID: org, ID: cur.ID}); err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "trace.destination.delete", TargetKind: "trace_destination", TargetID: cur.ID.String(),
			Before: auditView(toDestination(cur)),
		})
		return nil
	})
}

// Test sends the saved destination a synthetic trace again and keeps the result, only if the
// configuration is still the one that was tested: a destination deleted or changed meanwhile is
// ErrNotFound or ErrChanged, and nothing is written back. A failure is the outcome, not an error.
func (s *Service) Test(ctx context.Context, org pgtype.UUID, actor audit.Actor, id string) (Destination, export.Outcome, error) {
	if err := s.hold.Err(); err != nil {
		return Destination{}, export.Outcome{}, err
	}
	cur, err := s.row(ctx, store.New(s.pool), org, id)
	if err != nil {
		return Destination{}, export.Outcome{}, err
	}
	if err := outward.Check(cur.AdminDisabledAt); err != nil {
		return Destination{}, export.Outcome{}, err
	}
	if cur.CredentialLostAt.Valid {
		return Destination{}, export.Outcome{}, ErrCredentialLost
	}
	secrets, err := s.open(cur.Credentials, Binding(org, cur.ID))
	if err != nil {
		return Destination{}, export.Outcome{}, err
	}
	d := export.Destination{Preset: export.Preset(cur.Backend), Endpoint: cur.EndpointUrl, Params: decodeMap(cur.Params), Secrets: secrets}
	if err := s.policy.Check(ctx, d.Endpoint); err != nil {
		return Destination{}, export.Outcome{}, err
	}
	outcome, at := s.test(ctx, d, org.String(), decodeMap(cur.ResourceAttributes))
	testAt, testOK, testMsg := testColumns(outcome, at)
	var out Destination
	err = s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		n, err := q.SetTraceDestinationTest(ctx, store.SetTraceDestinationTestParams{
			OrgID: org, ID: cur.ID, ExpectedVersion: cur.ConfigVersion, LastTestAt: testAt, LastTestOk: testOK, LastTestMessage: testMsg,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			if _, err := s.row(ctx, q, org, id); err != nil {
				return err // deleted meanwhile: ErrNotFound
			}
			return ErrChanged
		}
		row, err := s.row(ctx, q, org, id)
		if err != nil {
			return err
		}
		out = toDestination(row)
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "trace.destination.test", TargetKind: "trace_destination", TargetID: out.ID,
			After: map[string]any{"ok": outcome.OK, "status": outcome.Status, "rejectedSpans": outcome.Rejected},
		})
		return nil
	})
	return out, outcome, err
}
