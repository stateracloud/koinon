// SPDX-License-Identifier: Apache-2.0

package githubtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The GitHub App manifest flow (issue #1175): the "new App" page that takes a manifest, the code
// conversion and the install page. The App the fake creates is the fake's own: AppID, PEM,
// FakeClientID and FakeClientSecret, installed as InstallationID.

// FakeAppSlug is the slug of the App the fake creates from a manifest.
const FakeAppSlug = "koinon-test-app"

// Manifest is the part of a GitHub App manifest the fake reads.
type Manifest struct {
	Name           string `json:"name"`
	URL            string `json:"url"`
	HookAttributes *struct {
		URL    string `json:"url"`
		Active bool   `json:"active"`
	} `json:"hook_attributes"`
	RedirectURL           string            `json:"redirect_url"`
	CallbackURLs          []string          `json:"callback_urls"`
	SetupURL              string            `json:"setup_url"`
	Public                bool              `json:"public"`
	DefaultPermissions    map[string]string `json:"default_permissions"`
	DefaultEvents         []string          `json:"default_events"`
	RequestOAuthOnInstall bool              `json:"request_oauth_on_install"`
}

// CreateFromManifest plays the admin who submits the manifest form to postURL (the form's action,
// GitHub's "new App" page) and creates the App. It makes code a valid manifest code and returns
// where GitHub sends the browser back: the manifest's redirect_url with code and state. It fails as
// GitHub would refuse the form.
func (f *Fake) CreateFromManifest(postURL, manifestJSON, code string) (string, error) {
	u, err := url.Parse(postURL)
	if err != nil {
		return "", err
	}
	if u.Scheme+"://"+u.Host != f.WebURL() {
		return "", fmt.Errorf("githubtest: %s is not the fake's web host", postURL)
	}
	orgPage := strings.HasPrefix(u.Path, "/organizations/") && strings.HasSuffix(u.Path, "/settings/apps/new")
	if u.Path != "/settings/apps/new" && !orgPage {
		return "", fmt.Errorf("githubtest: %s is not a new App page", postURL)
	}
	state := u.Query().Get("state")
	if state == "" {
		return "", fmt.Errorf("githubtest: %s has no state", postURL)
	}
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(manifestJSON))
	if err := dec.Decode(&m); err != nil {
		return "", fmt.Errorf("githubtest: the manifest is not valid JSON: %w", err)
	}
	if m.URL == "" || m.RedirectURL == "" {
		return "", fmt.Errorf("githubtest: the manifest needs url and redirect_url")
	}
	back, err := url.Parse(m.RedirectURL)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	if f.manifests == nil {
		f.manifests = map[string]Manifest{}
	}
	f.manifests[code] = m
	f.mu.Unlock()
	back.RawQuery = url.Values{"code": {code}, "state": {state}}.Encode()
	return back.String(), nil
}

// CreatedApp is the manifest of the App the fake created, if it created one.
func (f *Fake) CreatedApp() (Manifest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.created == nil {
		return Manifest{}, false
	}
	return *f.created, true
}

// Install plays the admin who installs the created App on the page at installURL. It returns the
// App's setup URL with the installation ID, setup_action and the state of installURL.
func (f *Fake) Install(installURL string) (string, error) {
	u, err := url.Parse(installURL)
	if err != nil {
		return "", err
	}
	if u.Scheme+"://"+u.Host != f.WebURL() || u.Path != "/apps/"+FakeAppSlug+"/installations/new" {
		return "", fmt.Errorf("githubtest: %s is not the install page of the created App", installURL)
	}
	m, ok := f.CreatedApp()
	if !ok || m.SetupURL == "" {
		return "", fmt.Errorf("githubtest: no App with a setup URL was created")
	}
	back, err := url.Parse(m.SetupURL)
	if err != nil {
		return "", err
	}
	q := url.Values{"installation_id": {strconv.FormatInt(f.InstallationID, 10)}, "setup_action": {"install"}}
	if s := u.Query().Get("state"); s != "" {
		q.Set("state", s)
	}
	back.RawQuery = q.Encode()
	return back.String(), nil
}

func (f *Fake) routeManifests(mux *http.ServeMux) {
	mux.HandleFunc("POST /app-manifests/{code}/conversions", f.convertManifest)
}

// convertManifest answers as GitHub does: 201 with the App and its secrets, once per code. It needs
// no authentication. The App's permissions become the manifest's.
func (f *Fake) convertManifest(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	code := r.PathValue("code")
	m, ok := f.manifests[code]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	delete(f.manifests, code)
	f.created = &m
	if len(m.DefaultPermissions) > 0 {
		f.Permissions = m.DefaultPermissions
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": f.AppID, "slug": FakeAppSlug, "name": m.Name, "html_url": f.WebURL() + "/apps/" + FakeAppSlug,
		"owner":     map[string]string{"login": f.Account},
		"client_id": FakeClientID, "client_secret": FakeClientSecret, "webhook_secret": nil, "pem": string(f.PEM),
		"permissions": f.Permissions,
	})
}
