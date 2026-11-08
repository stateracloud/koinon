// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/gitea"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/db/store"
	connectionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/connection/v1alpha1"
	"github.com/stateracloud/koinon/internal/orgsettings"
)

var tierToProto = map[connectors.Tier]connectionv1alpha1.Tier{
	connectors.TierRead:                   connectionv1alpha1.Tier_TIER_READ,
	connectors.TierReversibleWrite:        connectionv1alpha1.Tier_TIER_REVERSIBLE_WRITE,
	connectors.TierExternalOrIrreversible: connectionv1alpha1.Tier_TIER_EXTERNAL_OR_IRREVERSIBLE,
	connectors.TierForbidden:              connectionv1alpha1.Tier_TIER_FORBIDDEN,
}

var connectionStatusToProto = map[store.ConnectionStatus]connectionv1alpha1.ConnectionStatus{
	store.ConnectionStatusUntested: connectionv1alpha1.ConnectionStatus_CONNECTION_STATUS_UNTESTED,
	store.ConnectionStatusOk:       connectionv1alpha1.ConnectionStatus_CONNECTION_STATUS_OK,
	store.ConnectionStatusFailing:  connectionv1alpha1.ConnectionStatus_CONNECTION_STATUS_FAILING,
}

// ListConnectorTypes implements ConnectionService. Connector types are
// platform data, so any signed-in user may list them.
func (s *Server) ListConnectorTypes(ctx context.Context, _ *connectionv1alpha1.ListConnectorTypesRequest) (*connectionv1alpha1.ListConnectorTypesResponse, error) {
	if _, ok := auth.PrincipalFrom(ctx); !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, "login required")
	}
	types, err := connectors.All()
	if err != nil {
		return nil, s.dbError(err, "connector types")
	}
	res := &connectionv1alpha1.ListConnectorTypesResponse{}
	for _, t := range types {
		doc, err := toStruct(t)
		if err != nil {
			return nil, s.dbError(err, "connector type")
		}
		ct := &connectionv1alpha1.ConnectorType{
			Id: t.Metadata.ID, Version: t.Metadata.Version, DisplayName: t.Metadata.DisplayName,
			Description: t.Metadata.Description, AuthType: string(t.Spec.Auth.Type), Document: doc,
		}
		for _, sc := range t.Spec.Scopes {
			ct.Scopes = append(ct.Scopes, &connectionv1alpha1.Scope{
				Id: sc.ID, Tier: tierToProto[sc.Tier], Description: sc.Description, Tools: sc.Tools,
			})
		}
		res.ConnectorTypes = append(res.ConnectorTypes, ct)
	}
	return res, nil
}

func toStruct(v any) (*structpb.Struct, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return structpb.NewStruct(m)
}

func (s *Server) connectionProto(c store.Connection) *connectionv1alpha1.Connection {
	out := &connectionv1alpha1.Connection{
		Id: idString(c.ID), OrgId: idString(c.OrgID), Connector: c.Connector, DisplayName: c.DisplayName,
		Status: connectionStatusToProto[c.Status], LastTestedAt: tsProto(c.LastTestedAt), CreatedAt: tsProto(c.CreatedAt),
		Source: string(c.Source), ManagedBySecret: c.ManagedBySecret.String, Removed: c.RemovedAt.Valid,
		CredentialLostAt: tsProto(c.CredentialLostAt),
	}
	var results []connections.CheckResult
	if err := json.Unmarshal(c.LastTest, &results); err == nil {
		out.LastTest = checksProto(results)
	}
	if p, ok := s.hosts.Get(c.Connector); ok {
		switch p.ID() {
		case providers.GitHub:
			if d, err := p.Describe(c.Config); err == nil {
				out.Settings = &connectionv1alpha1.Connection_Github{Github: &connectionv1alpha1.GitHubConnection{
					AppId: d.AppID, InstallationId: d.InstallationID, KeyFingerprint: c.SecretFingerprint,
					BaseUrl: d.Site.BaseURL, CaBundle: d.Site.CABundle, CiDirs: d.Site.CIDirs,
				}}
			}
		case providers.Gitea:
			if d, err := p.Describe(c.Config); err == nil {
				out.Settings = &connectionv1alpha1.Connection_Gitea{Gitea: &connectionv1alpha1.GiteaConnection{
					BaseUrl: d.Site.BaseURL, CaBundle: d.Site.CABundle, CiDirs: d.Site.CIDirs,
					ReadTokenSetAt:        tsProto(pgtype.Timestamptz{Time: d.ReadTokenSetAt, Valid: !d.ReadTokenSetAt.IsZero()}),
					WriteTokenSetAt:       tsProto(pgtype.Timestamptz{Time: d.WriteTokenSetAt, Valid: !d.WriteTokenSetAt.IsZero()}),
					TokenWarnAfterSeconds: int64(p.Capabilities().TokenWarnAfter / time.Second),
				}}
			}
		}
	}
	return out
}

func checksProto(rs []connections.CheckResult) []*connectionv1alpha1.CheckResult {
	out := make([]*connectionv1alpha1.CheckResult, 0, len(rs))
	for _, r := range rs {
		out = append(out, &connectionv1alpha1.CheckResult{Check: r.Check, Step: r.Step, Ok: r.OK, Message: r.Message})
	}
	return out
}

// CreateGitHubConnection implements ConnectionService. The private key goes
// from the request straight into the secret store (ADR 0056): it is parsed (so a bad
// paste fails here, at the step that caused it), stored only as an envelope in
// Postgres, never returned and never logged; errors about it never quote it.
func (s *Server) CreateGitHubConnection(ctx context.Context, req *connectionv1alpha1.CreateGitHubConnectionRequest) (*connectionv1alpha1.CreateGitHubConnectionResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	// One request ID for the whole call, so the reading it starts audits under the same ID as the
	// call's own events (a UI request carries none).
	ctx = requestCtx(ctx)
	name := strings.TrimSpace(req.GetDisplayName())
	if name == "" {
		name = "GitHub"
	}
	switch {
	case len(name) > 200:
		return nil, connect.NewError(connect.CodeInvalidArgument, "display_name is longer than 200 characters")
	case req.GetAppId() <= 0:
		return nil, connect.NewError(connect.CodeInvalidArgument, "app_id must be the App's numeric ID")
	case req.GetInstallationId() <= 0:
		return nil, connect.NewError(connect.CodeInvalidArgument, "installation_id must be the installation's numeric ID")
	}
	baseURL, caBundle := strings.TrimSpace(req.GetBaseUrl()), strings.TrimSpace(req.GetCaBundle())
	// Take the key out of the request, so nothing that prints the request
	// later (protobuf-go ignores debug_redact) can show it.
	pemKey := []byte(req.GetPrivateKey())
	req.PrivateKey = ""
	host, ok := s.hosts.Get(providers.GitHub)
	if !ok {
		return nil, s.dbError(errors.New("github provider is not registered"), "connector type")
	}
	secrets := map[string][]byte{connections.GitHubPrivateKeyField: pemKey}
	if err := host.ValidateSecrets(secrets); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "private_key: "+strings.TrimPrefix(err.Error(), "github: "))
	}
	key, _ := github.ParsePrivateKey(pemKey) // validated above; only its fingerprint is used
	if s.keys == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, connections.ErrNoKeyring.Error())
	}
	t, ok := connectors.Get(host.ID())
	if !ok {
		return nil, s.dbError(errors.New("github connector type is not embedded"), "connector type")
	}

	loc, err := t.Locate(baseURL)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if loc.Default {
		baseURL = "" // github.com is the default; nothing to store
	} else {
		baseURL = loc.BaseURL()
		// The API and the reconciler send App JWTs to this host and the reconciler opens sandbox
		// egress to it: private and internal addresses only when the platform allow-lists them.
		if err := s.urlPolicy.Check(requestCtx(ctx), baseURL); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, "base_url: "+err.Error())
		}
	}
	if caBundle != "" {
		if _, err := codehost.CertPool(caBundle); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, "ca_bundle: "+err.Error())
		}
	}
	ciDirs, err := codehost.CleanCIDirs(req.GetCiDirs())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "ci_dirs: "+err.Error())
	}

	id, err := uuid.NewV7()
	if err != nil {
		return nil, s.dbError(err, "connection")
	}
	connID := pgtype.UUID{Bytes: id, Valid: true}
	config, err := json.Marshal(connections.GitHubConfig{
		AppID: req.GetAppId(), InstallationID: req.GetInstallationId(),
		Site: codehost.Site{BaseURL: baseURL, CABundle: caBundle, CIDirs: ciDirs},
	})
	if err != nil {
		return nil, s.dbError(err, "connection")
	}
	envelope, err := connections.SealFields(s.keys, c.org, connID, map[string][]byte{connections.GitHubPrivateKeyField: pemKey})
	if err != nil {
		s.log.Error("could not seal connection secret", "org", c.org.String(), "connection", connID.String(), "err", err)
		return nil, connect.NewError(connect.CodeUnavailable, "could not store the private key; try again")
	}
	var row store.Connection
	err = s.inTx(requestCtx(ctx), func(q *store.Queries) error {
		var err error
		row, err = q.CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
			OrgID: c.org, ID: connID, Connector: t.Metadata.ID, DisplayName: name, Config: config,
			SecretEnvelope: envelope, SecretFingerprint: key.Fingerprint(), Source: store.ConfigSourceUi, CreatedBy: c.actor(),
		})
		if err != nil {
			return err
		}
		return audit.RecordErr(requestCtx(ctx), q, audit.Event{
			Org: c.org, Actor: c.auditActor(), Action: "connection.create", TargetKind: "connection", TargetID: connID.String(),
			After: map[string]any{
				"connector": t.Metadata.ID, "displayName": name, "appId": req.GetAppId(),
				"installationId": req.GetInstallationId(), "keyFingerprint": key.Fingerprint(), "source": "ui",
				"baseUrl": baseURL, "caBundle": caBundle != "", "ciDirs": ciDirs,
			},
		})
	})
	if err != nil {
		return nil, s.dbError(err, "connection")
	}
	s.log.Info("connection created", "org", c.org.String(), "connection", connID.String(), "connector", t.Metadata.ID,
		"app_id", req.GetAppId(), "installation_id", req.GetInstallationId(), "key_fingerprint", key.Fingerprint(),
		"by", c.actor().String())

	res := &connectionv1alpha1.CreateGitHubConnectionResponse{}
	if req.GetTest() {
		row, res.Results, err = s.testConnection(ctx, row, c.auditActor())
		if err != nil {
			return nil, err
		}
	} else {
		s.syncScope(ctx, row, c.auditActor())
	}
	res.Connection = s.connectionProto(row)
	return res, nil
}

// CreateGiteaConnection implements ConnectionService. The two tokens go from the request straight
// into the secret store (ADR 0056): they are validated (so a bad paste fails here, at the step that
// caused it), stored only as an envelope in Postgres, never returned and never logged; errors about
// them never quote them. The connection's config holds the server's address, when each token was
// entered (Gitea does not expose a creation date) and a short fingerprint of each.
func (s *Server) CreateGiteaConnection(ctx context.Context, req *connectionv1alpha1.CreateGiteaConnectionRequest) (*connectionv1alpha1.CreateGiteaConnectionResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	// One request ID for the whole call, so the reading it starts audits under the same ID as the
	// call's own events (a UI request carries none).
	ctx = requestCtx(ctx)
	name := strings.TrimSpace(req.GetDisplayName())
	if name == "" {
		name = "Gitea"
	}
	if len(name) > 200 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "display_name is longer than 200 characters")
	}
	baseURL, caBundle := strings.TrimSpace(req.GetBaseUrl()), strings.TrimSpace(req.GetCaBundle())
	// Take the tokens out of the request, so nothing that prints the request later
	// (protobuf-go ignores debug_redact) can show them.
	readToken, writeToken := []byte(strings.TrimSpace(req.GetReadToken())), []byte(strings.TrimSpace(req.GetWriteToken()))
	req.ReadToken, req.WriteToken = "", ""
	host, ok := s.hosts.Get(providers.Gitea)
	if !ok {
		return nil, s.dbError(errors.New("gitea provider is not registered"), "connector type")
	}
	secrets := map[string][]byte{gitea.ReadTokenField: readToken, gitea.WriteTokenField: writeToken}
	if err := host.ValidateSecrets(secrets); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "tokens: "+strings.TrimPrefix(err.Error(), "gitea: "))
	}
	if s.keys == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, connections.ErrNoKeyring.Error())
	}
	t, ok := connectors.Get(host.ID())
	if !ok {
		return nil, s.dbError(errors.New("gitea connector type is not embedded"), "connector type")
	}
	loc, err := t.Locate(baseURL)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	baseURL = loc.BaseURL()
	// The API sends the tokens to this host and the reconciler opens sandbox egress to it: private
	// and internal addresses only when the platform allow-lists them.
	if err := s.urlPolicy.Check(requestCtx(ctx), baseURL); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "base_url: "+err.Error())
	}
	if caBundle != "" {
		if _, err := codehost.CertPool(caBundle); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, "ca_bundle: "+err.Error())
		}
	}
	ciDirs, err := codehost.CleanCIDirs(req.GetCiDirs())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "ci_dirs: "+err.Error())
	}

	id, err := uuid.NewV7()
	if err != nil {
		return nil, s.dbError(err, "connection")
	}
	connID := pgtype.UUID{Bytes: id, Valid: true}
	cfg := gitea.NewConfig(codehost.Site{BaseURL: baseURL, CABundle: caBundle, CIDirs: ciDirs}, readToken, writeToken, nil, time.Now())
	config, err := json.Marshal(cfg)
	if err != nil {
		return nil, s.dbError(err, "connection")
	}
	envelope, err := connections.SealFields(s.keys, c.org, connID, secrets)
	if err != nil {
		s.log.Error("could not seal connection secret", "org", c.org.String(), "connection", connID.String(), "err", err)
		return nil, connect.NewError(connect.CodeUnavailable, "could not store the tokens; try again")
	}
	fingerprint := cfg.ReadTokenFingerprint + "." + cfg.WriteTokenFingerprint
	var row store.Connection
	err = s.inTx(requestCtx(ctx), func(q *store.Queries) error {
		var err error
		row, err = q.CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
			OrgID: c.org, ID: connID, Connector: t.Metadata.ID, DisplayName: name, Config: config,
			SecretEnvelope: envelope, SecretFingerprint: fingerprint, Source: store.ConfigSourceUi, CreatedBy: c.actor(),
		})
		if err != nil {
			return err
		}
		return audit.RecordErr(requestCtx(ctx), q, audit.Event{
			Org: c.org, Actor: c.auditActor(), Action: "connection.create", TargetKind: "connection", TargetID: connID.String(),
			After: map[string]any{
				"connector": t.Metadata.ID, "displayName": name, "baseUrl": baseURL, "caBundle": caBundle != "",
				"ciDirs": ciDirs, "fingerprints": fingerprint, "source": "ui",
			},
		})
	})
	if err != nil {
		return nil, s.dbError(err, "connection")
	}
	s.log.Info("connection created", "org", c.org.String(), "connection", connID.String(), "connector", t.Metadata.ID,
		"token_fingerprints", fingerprint, "by", c.actor().String())

	res := &connectionv1alpha1.CreateGiteaConnectionResponse{}
	if req.GetTest() {
		row, res.Results, err = s.testConnection(ctx, row, c.auditActor())
		if err != nil {
			return nil, err
		}
	} else {
		s.syncScope(ctx, row, c.auditActor())
	}
	res.Connection = s.connectionProto(row)
	return res, nil
}

// SetConnectionSecret implements ConnectionService: a new secret for a connection entered here, by
// the rules of the create calls (validated first, sealed straight into an envelope, never returned,
// logged or quoted). It enters a secret lost with its key-encryption key again (ADR 0085 decision
// 13) and rotates one; the connection's other settings stay.
func (s *Server) SetConnectionSecret(ctx context.Context, req *connectionv1alpha1.SetConnectionSecretRequest) (*connectionv1alpha1.SetConnectionSecretResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	id, err := parseID("connection_id", req.GetConnectionId())
	if err != nil {
		return nil, err
	}
	// Take the secret out of the request, so nothing that prints the request later (protobuf-go
	// ignores debug_redact) can show it.
	var (
		connector, field string
		secrets          map[string][]byte
	)
	switch sec := req.GetSecret().(type) {
	case *connectionv1alpha1.SetConnectionSecretRequest_Github:
		connector, field = providers.GitHub, "private_key"
		secrets = map[string][]byte{connections.GitHubPrivateKeyField: []byte(sec.Github.GetPrivateKey())}
		sec.Github.PrivateKey = ""
	case *connectionv1alpha1.SetConnectionSecretRequest_Gitea:
		connector, field = providers.Gitea, "tokens"
		secrets = map[string][]byte{
			gitea.ReadTokenField:  []byte(strings.TrimSpace(sec.Gitea.GetReadToken())),
			gitea.WriteTokenField: []byte(strings.TrimSpace(sec.Gitea.GetWriteToken())),
		}
		sec.Gitea.ReadToken, sec.Gitea.WriteToken = "", ""
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, "secret is required: github or gitea")
	}
	host, ok := s.hosts.Get(connector)
	if !ok {
		return nil, s.dbError(fmt.Errorf("%s provider is not registered", connector), "connector type")
	}
	if err := host.ValidateSecrets(secrets); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, field+": "+strings.TrimPrefix(err.Error(), connector+": "))
	}
	if s.keys == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, connections.ErrNoKeyring.Error())
	}

	rctx := requestCtx(ctx)
	var row store.Connection
	err = s.inTx(rctx, func(q *store.Queries) error {
		cur, err := q.GetConnectionForUpdate(rctx, store.GetConnectionForUpdateParams{OrgID: c.org, ID: id})
		if err != nil {
			return err
		}
		switch {
		case cur.Connector != connector:
			return connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("this is a %s connection; send its %s secret", cur.Connector, cur.Connector))
		case cur.Source != store.ConfigSourceUi:
			return connect.NewError(connect.CodeFailedPrecondition, "this connection is declared by the Kubernetes Secret "+cur.ManagedBySecret.String+"; change the secret there")
		}
		config, fingerprint := cur.Config, ""
		switch connector {
		case providers.GitHub:
			key, _ := github.ParsePrivateKey(secrets[connections.GitHubPrivateKeyField]) // validated above
			fingerprint = key.Fingerprint()
		case providers.Gitea:
			prev, err := gitea.ParseConfig(cur.Config)
			if err != nil {
				return err
			}
			next := gitea.NewConfig(prev.Site, secrets[gitea.ReadTokenField], secrets[gitea.WriteTokenField], &prev, time.Now())
			if config, err = json.Marshal(next); err != nil {
				return err
			}
			fingerprint = next.ReadTokenFingerprint + "." + next.WriteTokenFingerprint
		}
		envelope, err := connections.SealFields(s.keys, c.org, cur.ID, secrets)
		if err != nil {
			s.log.Error("could not seal connection secret", "org", c.org.String(), "connection", cur.ID.String(), "err", err)
			return connect.NewError(connect.CodeUnavailable, "could not store the secret; try again")
		}
		row, err = q.SetUIConnectionSecret(rctx, store.SetUIConnectionSecretParams{
			OrgID: c.org, ID: cur.ID, Config: config, SecretEnvelope: envelope, SecretFingerprint: fingerprint,
		})
		if err != nil {
			return err
		}
		// What the old credential read is not what the new one reaches: the connection's part of
		// the code-host scope goes with it, and the sync below reads it again. The lock waits out a
		// replacement in progress, so its rows are deleted too.
		if err := orgsettings.ClearCodeHostScope(rctx, q, c.org, cur.ID); err != nil {
			return err
		}
		return audit.RecordErr(rctx, q, audit.Event{
			Org: c.org, Actor: c.auditActor(), Action: "connection.secret.set", TargetKind: "connection", TargetID: cur.ID.String(),
			Before: map[string]any{"displayName": cur.DisplayName, "fingerprint": cur.SecretFingerprint, "lost": cur.CredentialLostAt.Valid},
			After:  map[string]any{"displayName": cur.DisplayName, "fingerprint": fingerprint},
		})
	})
	if err != nil {
		return nil, s.dbError(err, "connection")
	}
	s.log.Info("connection secret set", "org", c.org.String(), "connection", row.ID.String(), "connector", row.Connector,
		"fingerprint", row.SecretFingerprint, "by", c.actor().String())

	res := &connectionv1alpha1.SetConnectionSecretResponse{}
	if req.GetTest() {
		tested, results, err := s.testConnection(rctx, row, c.auditActor())
		if err != nil {
			// The secret is stored and cannot be entered again by a retry that fails the same way:
			// answer with the stored connection and a failed test, not an error.
			s.log.Warn("test after storing a connection secret failed", "org", c.org.String(), "connection", row.ID.String(), "err", err)
			results = []*connectionv1alpha1.CheckResult{{Check: "test", Message: "the secret is stored, but Koinon could not run the checks; test the connection again"}}
		} else {
			row = tested
		}
		res.Results = results
	} else {
		s.syncScope(rctx, row, c.auditActor())
	}
	res.Connection = s.connectionProto(row)
	return res, nil
}

// DeleteConnection implements ConnectionService, by the rules of DeleteProvider: a connection entered
// here can be deleted, a declared one only after its Secret was deleted (removed), and none while an
// agent is bound to it.
func (s *Server) DeleteConnection(ctx context.Context, req *connectionv1alpha1.DeleteConnectionRequest) (*connectionv1alpha1.DeleteConnectionResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	id, err := parseID("connection_id", req.GetConnectionId())
	if err != nil {
		return nil, err
	}
	rctx := requestCtx(ctx)
	err = s.inAuditTx(rctx, func(q *store.Queries, b audit.Batch) error {
		// Lock order: the connection, then its linked MCP servers, as the link check
		// (mcpservers.lockLinkTarget) and the importer's unlink take them; the other order deadlocks.
		row, err := q.GetConnectionForUpdate(rctx, store.GetConnectionForUpdateParams{OrgID: c.org, ID: id})
		if err != nil {
			return err
		}
		if row.Source == store.ConfigSourceSecret && !row.RemovedAt.Valid {
			return connect.NewError(connect.CodeFailedPrecondition, "this connection is declared by the Kubernetes Secret "+row.ManagedBySecret.String+"; delete or change that Secret instead")
		}
		bound, err := q.ListAgentsBoundToConnection(rctx, store.ListAgentsBoundToConnectionParams{OrgID: c.org, ConnectionID: id.String()})
		if err != nil {
			return err
		}
		if len(bound) > 0 {
			names := make([]string, 0, len(bound))
			detail := &connectionv1alpha1.BoundAgents{}
			for _, agent := range bound {
				names = append(names, fmt.Sprintf("%s (%s)", agent.DisplayName, agent.ID.String()))
				detail.Agents = append(detail.Agents, &connectionv1alpha1.BoundAgent{Id: agent.ID.String(), DisplayName: agent.DisplayName})
			}
			// The message is for people and logs; the detail is what clients read.
			e := connect.NewError(connect.CodeFailedPrecondition, "agents are still bound to this connection; unbind them first: "+strings.Join(names, ", "))
			if d, err := connectproto.NewErrorDetail(detail); err == nil {
				e = e.WithDetail(d)
			}
			return e
		}
		// The foreign key unlinks the MCP servers of this connection (ON DELETE SET NULL); audit each
		// one, in this transaction, so the loss of the code-host link is never silent.
		linked, err := q.ListMCPServersLinkedToConnectionForUpdate(rctx, store.ListMCPServersLinkedToConnectionForUpdateParams{OrgID: c.org, ConnectionID: id})
		if err != nil {
			return err
		}
		if _, err := q.DeleteConnection(rctx, store.DeleteConnectionParams{OrgID: c.org, ID: id}); err != nil {
			return err
		}
		for _, srv := range linked {
			b.Org(audit.Event{
				Org: c.org, Actor: c.auditActor(), Action: "mcp_server.data_class.reset", TargetKind: "mcp_server", TargetID: srv.ID.String(),
				Before: map[string]any{"name": srv.Name, "codeHostConnectionId": id.String()},
				After:  map[string]any{"name": srv.Name, "codeHostConnectionId": nil, "reason": "connection_deleted"},
			})
		}
		b.Org(audit.Event{
			Org: c.org, Actor: c.auditActor(), Action: "connection.delete", TargetKind: "connection", TargetID: id.String(),
			Before: map[string]any{"displayName": row.DisplayName, "source": string(row.Source), "keyFingerprint": row.SecretFingerprint, "removed": row.RemovedAt.Valid},
		})
		return nil
	})
	if err != nil {
		return nil, s.dbError(err, "connection")
	}
	return &connectionv1alpha1.DeleteConnectionResponse{}, nil
}

// GetConnection implements ConnectionService.
func (s *Server) GetConnection(ctx context.Context, req *connectionv1alpha1.GetConnectionRequest) (*connectionv1alpha1.GetConnectionResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	id, err := parseID("connection_id", req.GetConnectionId())
	if err != nil {
		return nil, err
	}
	row, err := s.q.GetConnection(ctx, store.GetConnectionParams{OrgID: c.org, ID: id})
	if err != nil {
		return nil, s.dbError(err, "connection")
	}
	return &connectionv1alpha1.GetConnectionResponse{Connection: s.connectionProto(row)}, nil
}

// ListConnections implements ConnectionService.
func (s *Server) ListConnections(ctx context.Context, req *connectionv1alpha1.ListConnectionsRequest) (*connectionv1alpha1.ListConnectionsResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListConnections(ctx, c.org)
	if err != nil {
		return nil, s.dbError(err, "connections")
	}
	res := &connectionv1alpha1.ListConnectionsResponse{}
	for _, row := range rows {
		res.Connections = append(res.Connections, s.connectionProto(row))
	}
	return res, nil
}

// TestConnection implements ConnectionService.
func (s *Server) TestConnection(ctx context.Context, req *connectionv1alpha1.TestConnectionRequest) (*connectionv1alpha1.TestConnectionResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	// One request ID for the whole call, so the reading it starts audits under the same ID as the
	// call's own events (a UI request carries none).
	ctx = requestCtx(ctx)
	id, err := parseID("connection_id", req.GetConnectionId())
	if err != nil {
		return nil, err
	}
	row, err := s.q.GetConnection(ctx, store.GetConnectionParams{OrgID: c.org, ID: id})
	if err != nil {
		return nil, s.dbError(err, "connection")
	}
	row, results, err := s.testConnection(ctx, row, c.auditActor())
	if err != nil {
		return nil, err
	}
	return &connectionv1alpha1.TestConnectionResponse{Connection: s.connectionProto(row), Results: results}, nil
}

// testConnection runs the connector's checks and records them; when they pass, it reads the
// connection's repositories into the code-host scope as actor.
func (s *Server) testConnection(ctx context.Context, row store.Connection, actor audit.Actor) (store.Connection, []*connectionv1alpha1.CheckResult, error) {
	t, ok := connectors.Get(row.Connector)
	p, isHost := s.hosts.Get(row.Connector)
	if !ok || !isHost {
		return row, nil, connect.NewError(connect.CodeUnimplemented, "no checks for connector "+row.Connector)
	}
	var results []connections.CheckResult
	conn, err := connections.Load(ctx, s.keys, row)
	if err == nil {
		err = p.Validate(conn)
	}
	if err != nil {
		s.log.Warn("connection credentials unreadable", "org", row.OrgID.String(), "connection", row.ID.String(), "err", err)
		res := connections.CheckResult{Check: "secret", Step: "generate-key", Message: "Koinon could not read this connection's private key; create the connection again"}
		if p.ID() != providers.GitHub {
			res.Step, res.Message = "create-tokens", "Koinon could not read this connection's tokens or address; create the connection again"
		}
		if errors.Is(err, connections.ErrCredentialLost) {
			res.Message = "this connection's stored credential was lost with a key-encryption key; " + connections.LostRemedy(row)
		}
		results = []connections.CheckResult{res}
	} else {
		results = p.Checks(ctx, conn, t)
	}
	status := store.ConnectionStatusOk
	for _, r := range results {
		if !r.OK {
			status = store.ConnectionStatusFailing
		}
	}
	raw, err := json.Marshal(results)
	if err != nil {
		return row, nil, s.dbError(err, "connection test")
	}
	row, err = s.q.RecordConnectionTest(ctx, store.RecordConnectionTestParams{
		OrgID: row.OrgID, ID: row.ID, Status: status, LastTest: raw,
	})
	if err != nil {
		return row, nil, s.dbError(err, "connection")
	}
	s.log.Info("connection tested", "org", row.OrgID.String(), "connection", row.ID.String(), "status", string(status))
	if status == store.ConnectionStatusOk {
		s.syncScope(ctx, row, actor)
	}
	return row, checksProto(results), nil
}

// scopeSyncTimeout bounds the reading syncScope starts.
const scopeSyncTimeout = 2 * time.Minute

// syncScope reads the connection's repositories into the org's code-host scope (ADR 0091), so a
// write into one of the org's own private repositories is not external-high for long after the
// connection is created, re-keyed or tested. The reading runs in the background, detached from the
// request and bounded by scopeSyncTimeout: listing a large installation takes many pages, and the
// RPC must not wait for it (nor fail behind a proxy's timeout while it still runs). Nothing is
// lost by not waiting, because until the reading lands every code-host write on the connection is
// external-high. It is best effort: a failure leaves the stored scope to age out, which is the safe
// side, and the periodic refresh retries. actor is audited on the replacement.
func (s *Server) syncScope(ctx context.Context, row store.Connection, actor audit.Actor) {
	if s.scopeSync == nil {
		return
	}
	s.scopeJobs.start(ctx, row, actor)
}

// QueueScopeReading asks for a reading of the connection's repositories, as the periodic refresh
// does (scopesync.Syncer.SetQueue): it shares the one-reading-per-connection rule and Drain with
// the readings the RPCs start. The channel closes when a reading that covers the request has
// finished, or at once when the server is draining.
func (s *Server) QueueScopeReading(ctx context.Context, row store.Connection, actor audit.Actor) <-chan struct{} {
	return s.scopeJobs.start(ctx, row, actor)
}

// runScopeSync is one bounded reading, run by scopeJobs.
func (s *Server) runScopeSync(ctx context.Context, row store.Connection, actor audit.Actor) {
	ctx, cancel := context.WithTimeout(ctx, scopeSyncTimeout)
	defer cancel()
	if err := s.scopeSync.Sync(ctx, row, actor); err != nil {
		s.log.Warn("could not read the connection's repositories into the code-host scope", "org", row.OrgID.String(), "connection", row.ID.String(), "err", err)
	}
}

// Drain stops the background code-host scope readings and waits for the running ones, until ctx
// ends; then it cancels what still runs and waits for that to return. koinon-api calls it in its
// shutdown, inside the shutdown deadline. After Drain no reading starts.
func (s *Server) Drain(ctx context.Context) {
	s.scopeJobs.drain(ctx)
}

// Paging of ListInstallationRepos. The page token is GitHub's page number, in pages of
// installationReposPerPage repositories. A query is matched here, because GitHub's endpoint
// cannot filter: a filtered call reads GitHub pages until it has a page's worth of matches,
// the installation's repositories end, or it has read maxInstallationRepoPages pages.
const (
	installationReposPerPage = codehost.MaxPerPage
	maxInstallationRepoPages = 10
	maxInstallationRepoQuery = 200
)

// ListInstallationRepos implements ConnectionService.
func (s *Server) ListInstallationRepos(ctx context.Context, req *connectionv1alpha1.ListInstallationReposRequest) (*connectionv1alpha1.ListInstallationReposResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	id, err := parseID("connection_id", req.GetConnectionId())
	if err != nil {
		return nil, err
	}
	page := 1
	if t := req.GetPageToken(); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 1 || strconv.Itoa(n) != t {
			return nil, connect.NewError(connect.CodeInvalidArgument, "page_token is not a next_page_token of this call")
		}
		page = n
	}
	query := strings.ToLower(strings.TrimSpace(req.GetQuery()))
	if utf8.RuneCountInString(query) > maxInstallationRepoQuery {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("query is longer than %d characters", maxInstallationRepoQuery))
	}
	row, err := s.q.GetConnection(ctx, store.GetConnectionParams{OrgID: c.org, ID: id})
	if err != nil {
		return nil, s.dbError(err, "connection")
	}
	p, ok := s.hosts.Get(row.Connector)
	if !ok {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "the connection is not a code host connection")
	}
	name := p.Capabilities().DisplayName
	conn, err := connections.Load(ctx, s.keys, row)
	if err == nil {
		err = p.Validate(conn)
	}
	if err != nil {
		if errors.Is(err, connections.ErrNoKeyring) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
		}
		// The messages name the Secret or say the key is malformed; never its content.
		return nil, connect.NewError(connect.CodeFailedPrecondition, "the connection cannot authenticate to "+name+": "+err.Error())
	}
	// Metadata read only, the least any installation token has. No repositories named: the
	// token covers every repository of the installation, which is what is listed.
	acc, err := p.Open(ctx, conn, p.MetadataScope(nil))
	if err != nil {
		return nil, s.listReposError(row, name, err)
	}
	res := &connectionv1alpha1.ListInstallationReposResponse{}
	for read := 0; ; read++ {
		if read == maxInstallationRepoPages || (read > 0 && (query == "" || len(res.Repos) >= installationReposPerPage)) {
			res.NextPageToken = strconv.Itoa(page)
			break
		}
		got, err := acc.Repositories(ctx, page, installationReposPerPage)
		if err != nil {
			return nil, s.listReposError(row, name, err)
		}
		res.TotalCount = got.Total
		for _, r := range got.Repositories {
			if query == "" || strings.Contains(strings.ToLower(r.FullName), query) {
				res.Repos = append(res.Repos, &connectionv1alpha1.InstallationRepo{
					FullName: r.FullName, Private: r.Private, DefaultBranch: r.DefaultBranch,
				})
			}
		}
		if len(got.Repositories) == 0 || int64(page)*installationReposPerPage >= got.Total {
			break
		}
		page++
	}
	return res, nil
}

// listReposError maps an error from the code host while listing a connection's repositories.
func (s *Server) listReposError(conn store.Connection, name string, err error) error {
	switch {
	case isStatusError(err):
		return connect.NewError(connect.CodeFailedPrecondition, name+" refused the connection; test it for details: "+err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeCanceled, "request canceled")
	default:
		s.log.Warn("listing installation repositories failed", "connection", idString(conn.ID), "err", err)
		return connect.NewError(connect.CodeUnavailable, "could not reach "+name+" to list the repositories; try again")
	}
}

func isStatusError(err error) bool {
	_, ok := codehost.AsStatus(err)
	return ok
}
