// SPDX-License-Identifier: Apache-2.0

package gitea

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/codehost"
)

// pair is the two clients of a connection's checks.
type pair struct {
	read, write *client
	api         string
}

// checkFunc runs one built-in check (ADR 0021: referenced by name from the setup guide,
// implemented in Go).
type checkFunc func(ctx context.Context, p pair) (string, error)

var checks = map[string]checkFunc{
	"gitea.server":       checkServer,
	"gitea.tokens":       checkTokens,
	"gitea.repositories": checkRepositories,
	"gitea.read-token":   checkReadToken,
}

// Checks implements codehost.Provider: it runs the Gitea setup guide's checks in step order.
// After the first failure the remaining checks are reported as skipped. Nothing is written to
// Gitea.
func (h *Host) Checks(ctx context.Context, conn codehost.Connection, t *connectors.Type) []codehost.CheckResult {
	if err := h.Validate(conn); err != nil {
		return []codehost.CheckResult{{Check: "secret", Step: "create-tokens", Message: "Koinon could not read this connection's tokens or address; create the connection again"}}
	}
	rc, err := h.clientFor(conn, string(conn.Fields[ReadTokenField]))
	if err != nil {
		return []codehost.CheckResult{{Check: "host", Step: "create-bot", Message: describe(err)}}
	}
	wc, err := h.clientFor(conn, string(conn.Fields[WriteTokenField]))
	if err != nil {
		return []codehost.CheckResult{{Check: "host", Step: "create-bot", Message: describe(err)}}
	}
	p := pair{read: rc, write: wc, api: rc.base}
	var out []codehost.CheckResult
	failed := false
	for _, step := range t.Spec.SetupGuide.Steps {
		if step.Check == "" {
			continue
		}
		r := codehost.CheckResult{Check: step.Check, Step: step.ID}
		fn, ok := checks[step.Check]
		switch {
		case !ok:
			r.Message = "Koinon does not implement this check"
			failed = true
		case failed:
			r.Message = "skipped: an earlier check failed"
		default:
			msg, err := fn(ctx, p)
			if err != nil {
				r.Message = describe(err)
				failed = true
			} else {
				r.OK, r.Message = true, msg
			}
		}
		out = append(out, r)
	}
	return out
}

// describe turns an error into guidance for the step that caused it.
func describe(err error) string {
	var api *APIError
	if errors.As(err, &api) {
		switch api.Status {
		case http.StatusUnauthorized:
			return "Gitea rejected the token (" + api.Message + "). Check that it was copied whole and has not been deleted."
		case http.StatusForbidden:
			return "Gitea refused the request (" + api.Message + "). The token is probably missing a scope; see the token step."
		case http.StatusNotFound:
			return "Gitea answered 404 for " + api.Path + ". Check the server URL, including its path if Gitea is served under one."
		}
	}
	return err.Error()
}

func checkServer(ctx context.Context, p pair) (string, error) {
	v, err := p.read.version(ctx)
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", fmt.Errorf("%s did not answer like a Gitea server; check the URL", p.api)
	}
	return fmt.Sprintf("reached Gitea %s at %s", v, p.api), nil
}

func checkTokens(ctx context.Context, p pair) (string, error) {
	w, err := p.write.user(ctx)
	if err != nil {
		return "", fmt.Errorf("write token: %w", err)
	}
	r, err := p.read.user(ctx)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	if w.Login == "" || !strings.EqualFold(w.Login, r.Login) {
		return "", errors.New("the read token and the write token belong to different accounts; create both as the same bot")
	}
	return fmt.Sprintf("both tokens authenticate as the bot %s", w.Login), nil
}

// maxCheckedRepositories bounds the pages of the bot's repositories the check reads.
const maxCheckedRepositories = 500

// repositoriesShown bounds the repositories named in the check's message.
const repositoriesShown = 20

func level(p *apiPermissions) string {
	switch {
	case p == nil:
		return "unknown permission"
	case p.Admin:
		return "admin"
	case p.Push:
		return "write"
	case p.Pull:
		return "read only"
	}
	return "no access"
}

// listVisible reads the repositories a client's user can see.
func listVisible(ctx context.Context, c *client) ([]apiRepo, error) {
	var all []apiRepo
	for page := 1; len(all) < maxCheckedRepositories; page++ {
		got, _, err := c.userRepos(ctx, page, maxPerPage)
		if err != nil {
			return nil, err
		}
		all = append(all, got...)
		if len(got) < maxPerPage {
			break
		}
	}
	return all, nil
}

func checkRepositories(ctx context.Context, p pair) (string, error) {
	repos, err := listVisible(ctx, p.write)
	if err != nil {
		return "", err
	}
	if len(repos) == 0 {
		return "", errors.New("the bot cannot see any repository; add it as a collaborator with Write permission on each repository agents may work on")
	}
	var parts []string
	pushable, admin := 0, 0
	for i, r := range repos {
		if r.Permissions != nil && r.Permissions.Push {
			pushable++
		}
		if r.Permissions != nil && r.Permissions.Admin {
			admin++
		}
		if i < repositoriesShown {
			parts = append(parts, fmt.Sprintf("%s (%s)", r.FullName, level(r.Permissions)))
		}
	}
	if pushable == 0 {
		return "", fmt.Errorf("the bot can see %d repositories but cannot push to any (%s); give it Write permission", len(repos), strings.Join(parts, ", "))
	}
	msg := fmt.Sprintf("the bot can see %d repositories and push to %d: %s", len(repos), pushable, strings.Join(parts, ", "))
	if len(repos) > repositoriesShown {
		msg += fmt.Sprintf(", and %d more", len(repos)-repositoriesShown)
	}
	if admin > 0 {
		msg += fmt.Sprintf(". It is an administrator on %d; Write permission is enough", admin)
	}
	return msg + ". This should be exactly the repositories agents may work on.", nil
}

func checkReadToken(ctx context.Context, p pair) (string, error) {
	repos, err := listVisible(ctx, p.write)
	if err != nil {
		return "", err
	}
	// The probe POSTs to /issues, which answers 404 when a repository has issues disabled, so
	// prefer a repository that has them.
	var target *apiRepo
	for i := range repos {
		if repos[i].Permissions != nil && repos[i].Permissions.Push {
			if repos[i].HasIssues {
				target = &repos[i]
				break
			}
			if target == nil {
				target = &repos[i]
			}
		}
	}
	if target == nil {
		return "", errors.New("no repository the bot can push to to test the read token against")
	}
	owner, name, ok := strings.Cut(target.FullName, "/")
	if !ok {
		return "", fmt.Errorf("the server named a repository %q", target.FullName)
	}
	status, err := p.read.probeWrite(ctx, codehost.Repo{Owner: owner, Name: name})
	if err != nil {
		return "", err
	}
	switch status {
	case http.StatusForbidden:
		return fmt.Sprintf("Gitea refused a write to %s with the read token", target.FullName), nil
	case http.StatusUnauthorized:
		return "", errors.New("the server rejected the read token; check that it was copied whole")
	case http.StatusNotFound:
		return "", fmt.Errorf("the read token cannot see %s; it needs read:repository and read:issue", target.FullName)
	default:
		return "", fmt.Errorf("the read token was not refused a write to %s (Gitea answered %d): it has a write scope. Create a new read token with read scopes only", target.FullName, status)
	}
}
