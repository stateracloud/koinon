// SPDX-License-Identifier: Apache-2.0

package services

import (
	"crypto/rand"
	"encoding/base64"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/db/store"
)

// GitHubAppPostPath is the page that posts the manifest of a guided GitHub App setup to GitHub.
//
// GitHub's "new App" page takes the manifest only as a posted form field, and the SPA's CSP allows
// form posts to its own origin only (form-action 'self'). So the SPA posts the setup's state to this
// page, on its own origin. The page builds the manifest again from the setup and posts it to GitHub
// under a CSP whose form-action allows only that GitHub's origin. The SPA's CSP stays as it is.
const GitHubAppPostPath = "/auth/github/app-setup/post"

// RequestAuthenticator finds the signed-in user of a browser request. *auth.Service has it.
type RequestAuthenticator interface {
	AuthenticateRequest(r *http.Request) (*auth.Principal, error)
}

// MountGitHubAppPost registers the manifest post page.
func (s *Server) MountGitHubAppPost(mux *http.ServeMux, a RequestAuthenticator) {
	mux.HandleFunc("POST "+GitHubAppPostPath, func(w http.ResponseWriter, r *http.Request) {
		s.githubAppPost(w, r, a)
	})
}

var githubAppPostPage = template.Must(template.New("post").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Koinon: create the GitHub App</title></head>
<body style="font-family: system-ui, sans-serif; max-width: 40rem; margin: 3rem auto; padding: 0 1rem">
{{if .Error}}<h1>Koinon could not open GitHub</h1>
<p role="alert">{{.Error}}</p>
<p><a href="/connections/new/github">Start the setup again</a></p>
{{else}}<h1>Koinon opens GitHub</h1>
<form id="manifest" method="post" action="{{.PostURL}}">
<input type="hidden" name="manifest" value="{{.Manifest}}">
<p>If GitHub does not open, click the button.</p>
<button type="submit">Create the App on GitHub</button>
</form>
<script nonce="{{.Nonce}}">document.getElementById('manifest').submit()</script>
{{end}}</body></html>
`))

type githubAppPostData struct {
	PostURL  string
	Manifest string
	Nonce    string
	Error    string
}

func (s *Server) githubAppPost(w http.ResponseWriter, r *http.Request, a RequestAuthenticator) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	fail := func(status int, msg string) {
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_ = githubAppPostPage.Execute(w, githubAppPostData{Error: msg})
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		fail(http.StatusBadRequest, "The request is not valid.")
		return
	}
	p, err := a.AuthenticateRequest(r)
	if err != nil || p == nil || p.PasswordChangeRequired {
		fail(http.StatusUnauthorized, "Sign in to Koinon, then start the setup again.")
		return
	}
	state := r.PostForm.Get("state")
	orgPart, _, _ := strings.Cut(state, ".")
	c, err := authorize(auth.WithPrincipal(r.Context(), p), orgPart, store.OrgRoleAdmin)
	if err != nil {
		fail(http.StatusNotFound, "The GitHub App setup is unknown, used or expired.")
		return
	}
	ctx := requestCtx(r.Context())
	gh, t, publicURL, err := s.appSetupPrereqs()
	if err != nil {
		fail(http.StatusPreconditionFailed, "The guided GitHub App setup is not available on this install.")
		return
	}
	row, err := s.appSetup(ctx, c, state)
	if err != nil || row.AppID.Valid {
		// A setup whose App exists already continues on the install page, not here.
		fail(http.StatusNotFound, "The GitHub App setup is unknown, used or expired.")
		return
	}
	postURL, manifest, err := manifestPost(gh, t, publicURL, row, state)
	if err != nil {
		s.log.ErrorContext(ctx, "github app setup: build the manifest post", "org", c.org.String(), "error", err)
		fail(http.StatusInternalServerError, "Koinon could not build the App's manifest.")
		return
	}
	u, err := url.Parse(postURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		s.log.ErrorContext(ctx, "github app setup: the post URL is not valid", "org", c.org.String())
		fail(http.StatusInternalServerError, "Koinon could not build the App's manifest.")
		return
	}
	nonceBytes := make([]byte, 18)
	if _, err := rand.Read(nonceBytes); err != nil {
		fail(http.StatusInternalServerError, "Koinon could not build the page.")
		return
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'unsafe-inline'; "+
		"frame-ancestors 'none'; base-uri 'none'; form-action "+u.Scheme+"://"+u.Host)
	h.Set("Content-Type", "text/html; charset=utf-8")
	_ = githubAppPostPage.Execute(w, githubAppPostData{PostURL: postURL, Manifest: manifest, Nonce: nonce})
}
