// SPDX-License-Identifier: Apache-2.0

package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// The user authorization of an org's App (issue #1175, ADR 0005 amendment 2026-11-23): a Koinon
// user links their GitHub login once through the App's web flow. Koinon reads the login and the
// numeric user ID with the user token and then revokes the token at once; it never keeps it and
// never writes to the code host with it. The App's installation token authors every change.

// ClientSecretField is the stored secret field of the App's OAuth client secret. The manifest flow
// stores it with the private key; a connection without it cannot link users.
const ClientSecretField = "clientSecret"

// AuthorizeURL is the page where a user authorizes the App: the web flow with state and a PKCE
// S256 challenge.
func (c *Client) AuthorizeURL(clientID, redirectURI, state, challenge string) string {
	q := url.Values{
		"client_id": {clientID}, "redirect_uri": {redirectURI}, "state": {state},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "allow_signup": {"false"},
	}
	return c.webURL + "/login/oauth/authorize?" + q.Encode()
}

// UserCode is what the token exchange needs: the code GitHub gave the callback and the values
// that prove the same flow started it.
type UserCode struct {
	ClientID, ClientSecret, Code, RedirectURI, Verifier string
}

// ExchangeUserCode trades a user authorization code for a user access token. The token is
// returned only to read the user and must be revoked with RevokeUserToken; it is never stored
// or logged.
func (c *Client) ExchangeUserCode(ctx context.Context, in UserCode) (string, error) {
	form := url.Values{
		"client_id": {in.ClientID}, "client_secret": {in.ClientSecret}, "code": {in.Code},
		"redirect_uri": {in.RedirectURI}, "code_verifier": {in.Verifier},
	}
	const path = "/login/oauth/access_token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("github: POST %s: read response: %w", path, err)
	}
	// GitHub answers 200 with an error field for a bad or expired code.
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(data, &out) != nil {
		return "", &APIError{Method: http.MethodPost, Path: path, Status: resp.StatusCode, Message: "the token exchange failed"}
	}
	if out.Error != "" || out.AccessToken == "" {
		return "", &APIError{Method: http.MethodPost, Path: path, Status: http.StatusUnauthorized, Message: truncate(out.Error, 100)}
	}
	return out.AccessToken, nil
}

// User is the part of GET /user Koinon reads.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// AuthenticatedUser returns the user a user access token belongs to.
func (c *Client) AuthenticatedUser(ctx context.Context, userToken string) (User, error) {
	var out User
	err := c.userCall(ctx, http.MethodGet, "/user", "Bearer "+userToken, nil, &out, http.StatusOK)
	if err == nil && (out.ID <= 0 || out.Login == "") {
		err = errors.New("github: GET /user: no user in the response")
	}
	return out, err
}

// RevokeUserToken revokes a user access token of the App (DELETE /applications/{client_id}/token,
// authenticated with the App's client ID and secret).
func (c *Client) RevokeUserToken(ctx context.Context, clientID, clientSecret, userToken string) error {
	basic := "Basic " + basicAuth(clientID, clientSecret)
	return c.userCall(ctx, http.MethodDelete, "/applications/"+url.PathEscape(clientID)+"/token", basic,
		map[string]string{"access_token": userToken}, nil, http.StatusNoContent)
}

func basicAuth(user, pass string) string {
	r := &http.Request{Header: http.Header{}}
	r.SetBasicAuth(user, pass)
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Basic ")
}

// userCall is one REST call with an Authorization header that is not an installation token, or
// with none for an empty authorization (the manifest conversion).
func (c *Client) userCall(ctx context.Context, method, path, authorization string, body, out any, want int) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", APIVersion)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("github: %s %s: read response: %w", method, path, err)
	}
	if resp.StatusCode != want {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: truncate(e.Message, 300)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("github: %s %s: decode response: %w", method, path, err)
	}
	return nil
}

// RequestReviewers asks the users (logins) to review a pull request. It needs pull requests
// write; GitHub answers 422 when a user is not a collaborator of the repository.
func (c *Client) RequestReviewers(ctx context.Context, tok Token, r Repo, number int64, logins []string) error {
	return c.withToken(ctx, tok, http.MethodPost, r.path("/pulls/%d/requested_reviewers", number),
		map[string]any{"reviewers": logins}, nil, http.StatusCreated)
}
