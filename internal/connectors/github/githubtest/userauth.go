// SPDX-License-Identifier: Apache-2.0

package githubtest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// The App's user authorization (issue #1175): the token exchange, GET /user, the token revoke,
// and review requests on pull requests. The fake's OAuth client is ClientID and ClientSecret.

// FakeClientID and FakeClientSecret are the fake App's OAuth client. The secret is obviously fake.
const (
	FakeClientID     = "Iv1.koinontestclient"
	FakeClientSecret = "koinon_test_not-a-real-client-secret"
)

// GitHubUser is a user who can authorize the fake App.
type GitHubUser struct {
	ID    int64
	Login string
}

// Authorize makes code a valid authorization code for user, bound to the PKCE S256 challenge and
// the redirect URI of the authorize request. A code is valid once.
func (f *Fake) Authorize(code string, user GitHubUser, challenge, redirectURI string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.codes == nil {
		f.codes = map[string]authCode{}
	}
	f.codes[code] = authCode{user: user, challenge: challenge, redirect: redirectURI}
}

// Consent plays the user who approves the App on the authorize page at authorizeURL: it makes
// code valid for user and returns the URL GitHub sends the browser back to (the redirect URI with
// code and state). It fails when the page is not the fake App's.
func (f *Fake) Consent(authorizeURL, code string, user GitHubUser) (string, error) {
	u, err := url.Parse(authorizeURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if u.Path != "/login/oauth/authorize" || q.Get("client_id") != FakeClientID || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("state") == "" || q.Get("redirect_uri") == "" {
		return "", fmt.Errorf("githubtest: not an authorize URL of the fake App: %s", authorizeURL)
	}
	f.Authorize(code, user, q.Get("code_challenge"), q.Get("redirect_uri"))
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		return "", err
	}
	back.RawQuery = url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
	return back.String(), nil
}

// UserTokens returns the user tokens the fake issued and whether each is revoked.
func (f *Fake) UserTokens() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for t, u := range f.userTokens {
		out[t] = u.revoked
	}
	return out
}

// Reviewers returns the review requests of a pull request, in order.
func (f *Fake) Reviewers(repo string, number int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.repo(repo).Reviewers[number])
}

type authCode struct {
	user                GitHubUser
	challenge, redirect string
}

type userToken struct {
	user    GitHubUser
	revoked bool
}

func (f *Fake) routeUserAuth(mux *http.ServeMux) {
	mux.HandleFunc("POST /login/oauth/access_token", f.exchangeCode)
	mux.HandleFunc("GET /user", f.authenticatedUser)
	mux.HandleFunc("DELETE /applications/{client_id}/token", f.revokeToken)
	mux.HandleFunc("POST /repos/{owner}/{repo}/pulls/{number}/requested_reviewers", f.withRepo("pull_requests", "write", f.requestReviewers))
}

func (f *Fake) exchangeCode(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.PostForm.Get("client_id") != FakeClientID || r.PostForm.Get("client_secret") != FakeClientSecret {
		writeJSON(w, http.StatusOK, map[string]string{"error": "incorrect_client_credentials"})
		return
	}
	c, ok := f.codes[r.PostForm.Get("code")]
	delete(f.codes, r.PostForm.Get("code"))
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || c.challenge != base64.RawURLEncoding.EncodeToString(sum[:]) || c.redirect != r.PostForm.Get("redirect_uri") {
		writeJSON(w, http.StatusOK, map[string]string{"error": "bad_verification_code"})
		return
	}
	if f.userTokens == nil {
		f.userTokens = map[string]*userToken{}
	}
	tok := fmt.Sprintf("ghu_koinontestnotreal%d", len(f.userTokens)+1)
	f.userTokens[tok] = &userToken{user: c.user}
	writeJSON(w, http.StatusOK, map[string]string{"access_token": tok, "token_type": "bearer"})
}

func (f *Fake) authenticatedUser(w http.ResponseWriter, r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.userTokens[tok]
	if !ok || u.revoked {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": u.user.ID, "login": u.user.Login})
}

func (f *Fake) revokeToken(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if !ok || id != FakeClientID || secret != FakeClientSecret || r.PathValue("client_id") != FakeClientID {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	var in struct {
		AccessToken string `json:"access_token"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.userTokens[in.AccessToken]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	u.revoked = true
	w.WriteHeader(http.StatusNoContent)
}

// requestReviewers answers 422 for a login that is not in Collaborators, as GitHub does for a
// user who is not a collaborator. An empty Collaborators accepts every login.
func (f *Fake) requestReviewers(w http.ResponseWriter, req *http.Request, r *Repo) {
	n, _ := strconv.ParseInt(req.PathValue("number"), 10, 64)
	if n < 1 || n > int64(len(r.Pulls)) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	var in struct {
		Reviewers []string `json:"reviewers"`
	}
	if json.NewDecoder(req.Body).Decode(&in) != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
		return
	}
	for _, l := range in.Reviewers {
		if len(f.Collaborators) > 0 && !slices.Contains(f.Collaborators, l) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Reviews may only be requested from collaborators."})
			return
		}
	}
	if r.Reviewers == nil {
		r.Reviewers = map[int64][]string{}
	}
	r.Reviewers[n] = append(r.Reviewers[n], in.Reviewers...)
	writeJSON(w, http.StatusCreated, f.pullJSON(r.Pulls[n-1]))
}
