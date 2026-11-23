// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/codehost"
)

// The GitHub App manifest flow (issue #1175; GitHub docs, "Registering a GitHub App from a
// manifest"): Koinon builds a manifest, the admin's browser posts it to GitHub's "new App" page,
// GitHub creates the App and sends the browser back with a one-time code, and Koinon trades the
// code for the App's ID, private key and OAuth client (POST /app-manifests/{code}/conversions,
// unauthenticated, within one hour). Then the admin installs the App and GitHub sends the browser
// to the App's setup URL with the installation ID.

// Manifest is a GitHub App manifest.
type Manifest struct {
	Name           string          `json:"name"`
	URL            string          `json:"url"`
	Description    string          `json:"description,omitempty"`
	HookAttributes *HookAttributes `json:"hook_attributes,omitempty"`
	// RedirectURL is where GitHub sends the browser with the code after it created the App.
	RedirectURL string `json:"redirect_url"`
	// CallbackURLs are where a user's authorization of the App may send the browser back.
	CallbackURLs []string `json:"callback_urls"`
	// SetupURL is where GitHub sends the browser after an install, with the installation ID.
	SetupURL              string            `json:"setup_url"`
	Public                bool              `json:"public"`
	DefaultPermissions    map[string]string `json:"default_permissions"`
	DefaultEvents         []string          `json:"default_events"`
	RequestOAuthOnInstall bool              `json:"request_oauth_on_install"`
	SetupOnUpdate         bool              `json:"setup_on_update"`
}

// ManifestURLs are the Koinon pages a manifest names.
type ManifestURLs struct {
	// Home is Koinon's public URL, the App's homepage.
	Home string
	// Created receives the browser after GitHub created the App; Installed after an install.
	Created, Installed string
	// Callback receives the browser after a user authorized the App (the GitHub link).
	Callback string
}

// NewManifest is the manifest of an org's own App: private, with the permissions of the connector
// type's setup guide (never a forbidden one) and an inactive webhook, because Koinon polls.
func NewManifest(t *connectors.Type, name string, u ManifestURLs) Manifest {
	perms := make(map[string]string, len(t.Spec.Auth.Permissions))
	for p, level := range t.Spec.Auth.Permissions {
		perms[p] = level
	}
	return Manifest{
		Name: name, URL: u.Home,
		Description:    "Opens pull requests for the agents of a Koinon org. Each pull request names the person who asked for it.",
		HookAttributes: &HookAttributes{URL: u.Home, Active: false},
		RedirectURL:    u.Created, CallbackURLs: []string{u.Callback}, SetupURL: u.Installed,
		Public: false, DefaultPermissions: perms, DefaultEvents: []string{},
	}
}

// HookAttributes is a manifest's webhook. Koinon polls GitHub, so the webhook is inactive.
type HookAttributes struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// ManifestApp is what GitHub returns for a converted manifest code. It holds the App's private key
// and client secret: it formats and logs without them.
type ManifestApp struct {
	ID       int64
	Slug     string
	Name     string
	HTMLURL  string
	ClientID string
	Owner    string
	// PEM is the App's private key; ClientSecret its OAuth client secret. Never log them.
	PEM          []byte
	ClientSecret []byte
}

// String implements fmt.Stringer without the secrets.
func (a ManifestApp) String() string {
	return fmt.Sprintf("github.ManifestApp{ID:%d Slug:%q Owner:%q}", a.ID, a.Slug, a.Owner)
}

// GoString implements fmt.GoStringer without the secrets.
func (a ManifestApp) GoString() string { return a.String() }

// LogValue implements slog.LogValuer without the secrets.
func (a ManifestApp) LogValue() slog.Value {
	return slog.GroupValue(slog.Int64("id", a.ID), slog.String("slug", a.Slug), slog.String("owner", a.Owner))
}

// ManifestPostURL is the page the browser posts the manifest to: the "new App" page of owner (a
// GitHub organization) or, for an empty owner, of the signed-in user. GitHub sends state back.
func (c *Client) ManifestPostURL(owner, state string) string {
	path := "/settings/apps/new"
	if owner != "" {
		path = "/organizations/" + url.PathEscape(owner) + "/settings/apps/new"
	}
	return c.webURL + path + "?" + url.Values{"state": {state}}.Encode()
}

// InstallURL is the page where the admin installs the App slug. GitHub sends state on to the App's
// setup URL.
func (c *Client) InstallURL(slug, state string) string {
	return c.webURL + "/apps/" + url.PathEscape(slug) + "/installations/new?" + url.Values{"state": {state}}.Encode()
}

// ConvertManifest trades a manifest code for the App GitHub created. The code works once.
func (c *Client) ConvertManifest(ctx context.Context, code string) (ManifestApp, error) {
	if code == "" || strings.ContainsAny(code, "/?#") {
		return ManifestApp{}, errors.New("github: the manifest code is not valid")
	}
	var out struct {
		ID           int64  `json:"id"`
		Slug         string `json:"slug"`
		Name         string `json:"name"`
		HTMLURL      string `json:"html_url"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		PEM          string `json:"pem"`
		Owner        struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	err := c.userCall(ctx, http.MethodPost, "/app-manifests/"+url.PathEscape(code)+"/conversions", "", nil, &out, http.StatusCreated)
	if err != nil {
		return ManifestApp{}, err
	}
	if out.ID <= 0 || out.Slug == "" || out.ClientID == "" || out.ClientSecret == "" || out.PEM == "" {
		return ManifestApp{}, errors.New("github: the manifest conversion did not return the App's ID, slug, OAuth client and key")
	}
	return ManifestApp{
		ID: out.ID, Slug: out.Slug, Name: out.Name, HTMLURL: out.HTMLURL, ClientID: out.ClientID, Owner: out.Owner.Login,
		PEM: []byte(out.PEM), ClientSecret: []byte(out.ClientSecret),
	}, nil
}

// The Host's side of the manifest flow. conn carries only the site (base URL and CA bundle) in
// Config: the connection does not exist yet.

// ManifestPostURL is the "new App" page of the site's host.
func (h *Host) ManifestPostURL(conn codehost.Connection, owner, state string) (string, error) {
	c, err := h.clientFor(conn)
	if err != nil {
		return "", err
	}
	return c.ManifestPostURL(owner, state), nil
}

// InstallURL is the install page of the App slug on the site's host.
func (h *Host) InstallURL(conn codehost.Connection, slug, state string) (string, error) {
	c, err := h.clientFor(conn)
	if err != nil {
		return "", err
	}
	return c.InstallURL(slug, state), nil
}

// ConvertManifest trades a manifest code on the site's host.
func (h *Host) ConvertManifest(ctx context.Context, conn codehost.Connection, code string) (ManifestApp, error) {
	c, err := h.clientFor(conn)
	if err != nil {
		return ManifestApp{}, err
	}
	return c.ConvertManifest(ctx, code)
}

// ErrNotTheAppsInstallation means an installation ID does not belong to the App.
var ErrNotTheAppsInstallation = errors.New("github: the installation does not belong to the App")

// CheckInstallation makes sure installationID is an installation of the App (as the App: GitHub
// answers 404 for another App's installation).
func (h *Host) CheckInstallation(ctx context.Context, conn codehost.Connection, appID, installationID int64, pemKey []byte) error {
	c, err := h.clientFor(conn)
	if err != nil {
		return err
	}
	key, err := ParsePrivateKey(pemKey)
	if err != nil {
		return err
	}
	inst, err := c.GetInstallation(ctx, App{AppID: appID, InstallationID: installationID, Key: key})
	var api *APIError
	switch {
	case errors.As(err, &api) && api.Status == http.StatusNotFound:
		return ErrNotTheAppsInstallation
	case err != nil:
		return err
	case inst.AppID != 0 && inst.AppID != appID:
		return ErrNotTheAppsInstallation
	}
	return nil
}
