// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	connect "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/procoduck/koinon/connectors"
	"github.com/procoduck/koinon/internal/auth"
	"github.com/procoduck/koinon/internal/connections"
	"github.com/procoduck/koinon/internal/connectors/github"
	"github.com/procoduck/koinon/internal/db/store"
	connectionv1alpha1 "github.com/procoduck/koinon/internal/gen/koinon/connection/v1alpha1"
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
	}
	var results []connections.CheckResult
	if err := json.Unmarshal(c.LastTest, &results); err == nil {
		out.LastTest = checksProto(results)
	}
	if c.Connector == connectors.GitHubID {
		if cfg, err := connections.ParseGitHubConfig(c.Config); err == nil {
			out.Settings = &connectionv1alpha1.Connection_Github{Github: &connectionv1alpha1.GitHubConnection{
				AppId: cfg.AppID, InstallationId: cfg.InstallationID, KeyFingerprint: c.SecretFingerprint,
			}}
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
// from the request straight into a Kubernetes Secret: it is parsed (so a bad
// paste fails here, at the step that caused it), never stored in Postgres,
// never returned and never logged; errors about it never quote it.
func (s *Server) CreateGitHubConnection(ctx context.Context, req *connectionv1alpha1.CreateGitHubConnectionRequest) (*connectionv1alpha1.CreateGitHubConnectionResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
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
	// Take the key out of the request, so nothing that prints the request
	// later (protobuf-go ignores debug_redact) can show it.
	pemKey := []byte(req.GetPrivateKey())
	req.PrivateKey = ""
	key, err := github.ParsePrivateKey(pemKey)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "private_key: "+strings.TrimPrefix(err.Error(), "github: "))
	}
	if s.secrets == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, connections.ErrNoSecretStore.Error())
	}
	t, ok := connectors.Get(connectors.GitHubID)
	if !ok {
		return nil, s.dbError(errors.New("github connector type is not embedded"), "connector type")
	}

	id, err := uuid.NewV7()
	if err != nil {
		return nil, s.dbError(err, "connection")
	}
	connID := pgtype.UUID{Bytes: id, Valid: true}
	secretName := connections.SecretName(c.org.Bytes, connID.Bytes)
	config, err := json.Marshal(connections.GitHubConfig{AppID: req.GetAppId(), InstallationID: req.GetInstallationId()})
	if err != nil {
		return nil, s.dbError(err, "connection")
	}

	// Secret first: a row without its Secret would be a connection that can
	// never work. If the row fails, the Secret is removed again.
	if err := s.secrets.Create(ctx, connections.Secret{
		Name: secretName,
		Labels: map[string]string{
			connections.LabelManagedBy:  connections.ManagedByValue,
			connections.LabelOrg:        c.org.String(),
			connections.LabelConnection: connID.String(),
		},
		Annotations: map[string]string{
			connections.AnnotationConnector:   t.Metadata.ID,
			connections.AnnotationFingerprint: key.Fingerprint(),
		},
		Data: map[string][]byte{connections.GitHubPrivateKeyField: pemKey},
	}); err != nil {
		s.log.Error("could not write connection secret", "org", c.org.String(), "connection", connID.String(), "err", err)
		return nil, connect.NewError(connect.CodeUnavailable, "could not store the private key; try again")
	}
	row, err := s.q.CreateConnection(ctx, store.CreateConnectionParams{
		OrgID: c.org, ID: connID, Connector: t.Metadata.ID, DisplayName: name, Config: config,
		SecretName: secretName, SecretFingerprint: key.Fingerprint(), CreatedBy: c.actor(),
	})
	if err != nil {
		if derr := s.secrets.Delete(context.WithoutCancel(ctx), secretName); derr != nil {
			s.log.Error("could not remove the Secret of a connection that was not created", "secret", secretName, "err", derr)
		}
		return nil, s.dbError(err, "connection")
	}
	s.log.Info("connection created", "org", c.org.String(), "connection", connID.String(), "connector", t.Metadata.ID,
		"app_id", req.GetAppId(), "installation_id", req.GetInstallationId(), "key_fingerprint", key.Fingerprint(),
		"by", c.actor().String())

	res := &connectionv1alpha1.CreateGitHubConnectionResponse{}
	if req.GetTest() {
		row, res.Results, err = s.testConnection(ctx, row)
		if err != nil {
			return nil, err
		}
	}
	res.Connection = s.connectionProto(row)
	return res, nil
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
	id, err := parseID("connection_id", req.GetConnectionId())
	if err != nil {
		return nil, err
	}
	row, err := s.q.GetConnection(ctx, store.GetConnectionParams{OrgID: c.org, ID: id})
	if err != nil {
		return nil, s.dbError(err, "connection")
	}
	row, results, err := s.testConnection(ctx, row)
	if err != nil {
		return nil, err
	}
	return &connectionv1alpha1.TestConnectionResponse{Connection: s.connectionProto(row), Results: results}, nil
}

func (s *Server) testConnection(ctx context.Context, row store.Connection) (store.Connection, []*connectionv1alpha1.CheckResult, error) {
	t, ok := connectors.Get(row.Connector)
	if !ok || row.Connector != connectors.GitHubID {
		return row, nil, connect.NewError(connect.CodeUnimplemented, "no checks for connector "+row.Connector)
	}
	var results []connections.CheckResult
	app, err := connections.LoadGitHubApp(ctx, s.secrets, row)
	if err != nil {
		s.log.Warn("connection credentials unreadable", "org", row.OrgID.String(), "connection", row.ID.String(), "err", err)
		results = []connections.CheckResult{{Check: "secret", Step: "generate-key", Message: "Koinon could not read this connection's private key; create the connection again"}}
	} else {
		results = connections.TestGitHub(ctx, s.github, t, app)
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
	return row, checksProto(results), nil
}
