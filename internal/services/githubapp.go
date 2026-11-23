// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	connect "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/db/store"
	connectionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/connection/v1alpha1"
	"github.com/stateracloud/koinon/internal/githublink"
)

// The guided GitHub App setup (issue #1175). Each org has its own GitHub App, created from a
// manifest Koinon builds:
//
//  1. StartGitHubAppSetup stores a setup under a one-time state (only its hash) and returns the
//     manifest and GitHub's "new App" page. The browser posts the manifest there.
//  2. GitHub creates the App and sends the browser to /connections/new/github/app-created with a
//     code. ContinueGitHubAppSetup trades the code for the App's private key and OAuth client and
//     seals both at once, under the connection ID the setup reserved: they never leave the API
//     server unsealed. It returns the App's install page.
//  3. GitHub sends the browser to /connections/new/github/app-installed with the installation ID.
//     FinishGitHubAppSetup checks, as the App, that the installation is the App's, makes the
//     connection with the sealed envelope and tests it.
//
// Every step is the same admin's, in the same org, within GitHubAppSetupTTL of the last step.

// GitHubAppSetupTTL is how long a setup waits for its next step.
const GitHubAppSetupTTL = time.Hour

// The Koinon pages GitHub sends the browser to.
const (
	githubAppCreatedPath   = "/connections/new/github/app-created"
	githubAppInstalledPath = "/connections/new/github/app-installed"
)

// githubAppNameMax is GitHub's limit for an App's name.
const githubAppNameMax = 34

// githubLogin is a GitHub user or organization login.
var githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// githubAppSetupHost is the GitHub provider's side of the manifest flow.
type githubAppSetupHost interface {
	ManifestPostURL(conn codehost.Connection, owner, state string) (string, error)
	InstallURL(conn codehost.Connection, slug, state string) (string, error)
	ConvertManifest(ctx context.Context, conn codehost.Connection, code string) (github.ManifestApp, error)
	CheckInstallation(ctx context.Context, conn codehost.Connection, appID, installationID int64, pemKey []byte) error
}

// appSetupPrereqs returns the GitHub provider and Koinon's public URL, or why the guided setup
// cannot run on this install.
func (s *Server) appSetupPrereqs() (githubAppSetupHost, *connectors.Type, string, error) {
	if s.githubLinks == nil {
		return nil, nil, "", connect.NewError(connect.CodeUnimplemented, "the guided GitHub App setup is not available on this install")
	}
	host, ok := s.hosts.Get(providers.GitHub)
	if !ok {
		return nil, nil, "", s.dbError(errors.New("github provider is not registered"), "connector type")
	}
	h, ok := host.(githubAppSetupHost)
	if !ok {
		return nil, nil, "", connect.NewError(connect.CodeUnimplemented, "the GitHub provider cannot set up an App")
	}
	t, ok := connectors.Get(host.ID())
	if !ok {
		return nil, nil, "", s.dbError(errors.New("github connector type is not embedded"), "connector type")
	}
	publicURL := strings.TrimRight(s.githubLinks.PublicURL(), "/")
	if publicURL == "" {
		return nil, nil, "", connect.NewError(connect.CodeFailedPrecondition, "Koinon has no public URL, so GitHub cannot send you back; an operator sets it")
	}
	if s.keys == nil {
		return nil, nil, "", connect.NewError(connect.CodeFailedPrecondition, connections.ErrNoKeyring.Error())
	}
	return h, t, publicURL, nil
}

// setupSite is the connection-to-be that carries only the site, for the provider's URLs.
func setupSite(row store.GithubAppSetup, connector string) codehost.Connection {
	cfg, _ := json.Marshal(connections.GitHubConfig{Site: codehost.Site{BaseURL: row.BaseUrl, CABundle: row.CaBundle}})
	return codehost.Connection{ID: row.ConnectionID.String(), DisplayName: row.DisplayName, Connector: connector, Config: cfg}
}

func stateHash(state string) []byte {
	h := sha256.Sum256([]byte(state))
	return h[:]
}

func errSetupNotFound() error {
	return connect.NewError(connect.CodeNotFound, "the GitHub App setup is unknown, used or expired; start it again")
}

func (s *Server) StartGitHubAppSetup(ctx context.Context, req *connectionv1alpha1.StartGitHubAppSetupRequest) (*connectionv1alpha1.StartGitHubAppSetupResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	if err := s.refuseWhileHeld(); err != nil {
		return nil, err
	}
	ctx = requestCtx(ctx)
	h, t, publicURL, err := s.appSetupPrereqs()
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.GetDisplayName())
	owner := strings.TrimSpace(req.GetGithubOwner())
	baseURL, caBundle := strings.TrimSpace(req.GetBaseUrl()), strings.TrimSpace(req.GetCaBundle())
	switch {
	case name == "" || utf8.RuneCountInString(name) > githubAppNameMax:
		return nil, connect.NewError(connect.CodeInvalidArgument, "display_name is the App's name on GitHub: 1 to 34 characters, unique on GitHub")
	case owner != "" && !githubLogin.MatchString(owner):
		return nil, connect.NewError(connect.CodeInvalidArgument, "github_owner must be a GitHub organization login")
	}
	loc, err := t.Locate(baseURL)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if loc.Default {
		baseURL = ""
	} else {
		baseURL = loc.BaseURL()
		if err := s.urlPolicy.CheckSandbox(ctx, baseURL); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, "base_url: "+err.Error())
		}
	}
	if caBundle != "" {
		if _, err := codehost.CertPool(caBundle); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, "ca_bundle: "+err.Error())
		}
	}

	id, err := uuid.NewV7()
	if err != nil {
		return nil, s.dbError(err, "GitHub App setup")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, s.dbError(err, "GitHub App setup")
	}
	// The state names the org, so the pages GitHub sends the browser to know which org it is.
	state := c.org.String() + "." + base64.RawURLEncoding.EncodeToString(secret)
	row := store.GithubAppSetup{
		OrgID: c.org, StateHash: stateHash(state), UserID: c.actor(), ConnectionID: pgtype.UUID{Bytes: id, Valid: true},
		DisplayName: name, BaseUrl: baseURL, CaBundle: caBundle, GithubOwner: owner,
	}
	postURL, err := h.ManifestPostURL(setupSite(row, t.Metadata.ID), owner, state)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	manifest, err := json.Marshal(github.NewManifest(t, name, github.ManifestURLs{
		Home: publicURL, Created: publicURL + githubAppCreatedPath, Installed: publicURL + githubAppInstalledPath,
		Callback: publicURL + githublink.CallbackPath,
	}))
	if err != nil {
		return nil, s.dbError(err, "GitHub App setup")
	}
	now := time.Now()
	err = s.inTx(ctx, func(q *store.Queries) error {
		if err := q.DeleteExpiredGitHubAppSetups(ctx, store.DeleteExpiredGitHubAppSetupsParams{OrgID: c.org, Now: pgtype.Timestamptz{Time: now, Valid: true}}); err != nil {
			return err
		}
		return q.CreateGitHubAppSetup(ctx, store.CreateGitHubAppSetupParams{
			OrgID: row.OrgID, StateHash: row.StateHash, UserID: row.UserID, ConnectionID: row.ConnectionID,
			DisplayName: name, BaseUrl: baseURL, CaBundle: caBundle, GithubOwner: owner,
			ExpiresAt: pgtype.Timestamptz{Time: now.Add(GitHubAppSetupTTL), Valid: true},
		})
	})
	if err != nil {
		return nil, s.dbError(err, "GitHub App setup")
	}
	return &connectionv1alpha1.StartGitHubAppSetupResponse{PostUrl: postURL, Manifest: string(manifest), State: state}, nil
}

// appSetup loads the caller's live setup of state.
func (s *Server) appSetup(ctx context.Context, c caller, state string) (store.GithubAppSetup, error) {
	if state == "" || len(state) > 200 {
		return store.GithubAppSetup{}, errSetupNotFound()
	}
	row, err := s.q.GetGitHubAppSetup(ctx, store.GetGitHubAppSetupParams{
		OrgID: c.org, StateHash: stateHash(state), UserID: c.actor(), Now: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.GithubAppSetup{}, errSetupNotFound()
	}
	if err != nil {
		return store.GithubAppSetup{}, s.dbError(err, "GitHub App setup")
	}
	return row, nil
}

func (s *Server) ContinueGitHubAppSetup(ctx context.Context, req *connectionv1alpha1.ContinueGitHubAppSetupRequest) (*connectionv1alpha1.ContinueGitHubAppSetupResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	if err := s.refuseWhileHeld(); err != nil {
		return nil, err
	}
	ctx = requestCtx(ctx)
	h, t, _, err := s.appSetupPrereqs()
	if err != nil {
		return nil, err
	}
	row, err := s.appSetup(ctx, c, req.GetState())
	if err != nil {
		return nil, err
	}
	site := setupSite(row, t.Metadata.ID)
	install := func(slug string) (*connectionv1alpha1.ContinueGitHubAppSetupResponse, error) {
		u, err := h.InstallURL(site, slug, req.GetState())
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
		}
		return &connectionv1alpha1.ContinueGitHubAppSetupResponse{InstallUrl: u, AppSlug: slug}, nil
	}
	if row.AppID.Valid {
		// A reload of the page: GitHub's code works once, and the App is already stored.
		return install(row.AppSlug.String)
	}
	code := strings.TrimSpace(req.GetCode())
	if code == "" || len(code) > 200 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "code must be the code GitHub sent back")
	}
	app, err := h.ConvertManifest(ctx, site, code)
	if err != nil {
		var api *github.APIError
		if errors.As(err, &api) && (api.Status == http.StatusNotFound || api.Status == http.StatusUnprocessableEntity) {
			return nil, connect.NewError(connect.CodeInvalidArgument, "GitHub did not accept the code: it works once and for one hour; start the setup again")
		}
		s.log.Warn("github app manifest conversion failed", "org", c.org.String(), "err", err)
		return nil, connect.NewError(connect.CodeUnavailable, "could not get the App from GitHub; try again")
	}
	envelope, err := connections.SealFields(s.keys, c.org, row.ConnectionID, map[string][]byte{
		connections.GitHubPrivateKeyField: app.PEM, github.ClientSecretField: app.ClientSecret,
	})
	if err != nil {
		s.log.Error("could not seal the GitHub App's secrets", "org", c.org.String(), "connection", row.ConnectionID.String(), "err", err)
		return nil, connect.NewError(connect.CodeUnavailable, "could not store the App's private key; try again")
	}
	err = s.inAuditTx(ctx, func(q *store.Queries, b audit.Batch) error {
		n, err := q.SetGitHubAppSetupApp(ctx, store.SetGitHubAppSetupAppParams{
			AppID: pgtype.Int8{Int64: app.ID, Valid: true}, AppSlug: pgtype.Text{String: app.Slug, Valid: true},
			ClientID: pgtype.Text{String: app.ClientID, Valid: true}, SecretEnvelope: envelope,
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(GitHubAppSetupTTL), Valid: true},
			OrgID:     c.org, StateHash: row.StateHash,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return connect.NewError(connect.CodeAborted, "another request finished this step of the setup; reload the page")
		}
		b.Org(audit.Event{
			Org: c.org, Actor: c.auditActor(), Action: "github_app.create", TargetKind: "connection", TargetID: row.ConnectionID.String(),
			After: map[string]any{
				"appId": app.ID, "appSlug": app.Slug, "clientId": app.ClientID, "owner": app.Owner,
				"baseUrl": row.BaseUrl, "displayName": row.DisplayName,
			},
		})
		return nil
	})
	if err != nil {
		return nil, s.dbError(err, "GitHub App setup")
	}
	s.log.Info("github app created from manifest", "org", c.org.String(), "connection", row.ConnectionID.String(),
		"app", app, "by", c.actor().String())
	return install(app.Slug)
}

func (s *Server) FinishGitHubAppSetup(ctx context.Context, req *connectionv1alpha1.FinishGitHubAppSetupRequest) (*connectionv1alpha1.FinishGitHubAppSetupResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	if err := s.refuseWhileHeld(); err != nil {
		return nil, err
	}
	ctx = requestCtx(ctx)
	h, t, _, err := s.appSetupPrereqs()
	if err != nil {
		return nil, err
	}
	row, err := s.appSetup(ctx, c, req.GetState())
	if err != nil {
		return nil, err
	}
	if !row.AppID.Valid {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "GitHub has not created the App of this setup yet")
	}
	if req.GetInstallationId() <= 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "installation_id must be the installation's numeric ID")
	}
	fields, err := connections.OpenFields(s.keys, store.Connection{OrgID: row.OrgID, ID: row.ConnectionID, SecretEnvelope: row.SecretEnvelope})
	if err != nil {
		s.log.Error("could not open the GitHub App setup's secrets", "org", c.org.String(), "connection", row.ConnectionID.String(), "err", err)
		return nil, connect.NewError(connect.CodeFailedPrecondition, "could not open the App's stored key; start the setup again")
	}
	pemKey := fields[connections.GitHubPrivateKeyField]
	// The installation ID comes from the browser: make sure, as the App, that it is the App's.
	err = h.CheckInstallation(ctx, setupSite(row, t.Metadata.ID), row.AppID.Int64, req.GetInstallationId(), pemKey)
	switch {
	case errors.Is(err, github.ErrNotTheAppsInstallation):
		return nil, connect.NewError(connect.CodeInvalidArgument, "the installation does not belong to the App of this setup")
	case err != nil:
		s.log.Warn("github app installation check failed", "org", c.org.String(), "err", err)
		return nil, connect.NewError(connect.CodeUnavailable, "could not check the installation with GitHub; try again")
	}
	res, err := s.createGitHub(ctx, c, githubCreate{
		name: row.DisplayName, appID: row.AppID.Int64, installationID: req.GetInstallationId(), pemKey: pemKey,
		baseURL: row.BaseUrl, caBundle: row.CaBundle, test: true,
		connID: row.ConnectionID, envelope: row.SecretEnvelope, clientID: row.ClientID.String, appSlug: row.AppSlug.String,
	})
	if err != nil {
		return nil, err
	}
	// The setup is used: its state works no more. The connection exists, so a failure here only
	// leaves a row that expires.
	if _, err := s.q.DeleteGitHubAppSetup(ctx, store.DeleteGitHubAppSetupParams{OrgID: c.org, StateHash: row.StateHash}); err != nil {
		s.log.Warn("could not delete a used GitHub App setup", "org", c.org.String(), "err", err)
	}
	return &connectionv1alpha1.FinishGitHubAppSetupResponse{Connection: res.GetConnection(), Results: res.GetResults()}, nil
}
