// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/codehost"
)

// checkFunc runs one built-in check (ADR 0021: referenced by name from the setup guide,
// implemented in Go).
type checkFunc func(ctx context.Context, c *Client, t *connectors.Type, app App) (string, error)

var checks = map[string]checkFunc{
	"github.app":          checkApp,
	"github.installation": checkInstallation,
	"github.token":        checkToken,
}

// Checks implements codehost.Provider: it runs the GitHub setup guide's checks in step order.
// After the first failure the remaining checks are reported as skipped.
func (h *Host) Checks(ctx context.Context, conn codehost.Connection, t *connectors.Type) []codehost.CheckResult {
	app, err := AppOf(conn)
	if err != nil {
		return []codehost.CheckResult{{Check: "secret", Step: "generate-key", Message: "Koinon could not read this connection's private key; create the connection again"}}
	}
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
			msg, err := fn(ctx, h.c, t, app)
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
		case 401:
			return fmt.Sprintf("GitHub rejected the App's credentials (%s). Check the App ID and that the private key belongs to this App and was not deleted.", api.Message)
		case 404:
			return "GitHub found no such installation of this App. Check the installation ID and that the App is installed."
		}
	}
	return err.Error()
}

func checkApp(ctx context.Context, c *Client, _ *connectors.Type, app App) (string, error) {
	info, err := c.GetApp(ctx, app)
	if err != nil {
		return "", err
	}
	if info.ID != app.AppID {
		return "", fmt.Errorf("GitHub returned App %d, not %d", info.ID, app.AppID)
	}
	return fmt.Sprintf("authenticated as the App %q (ID %d) with the key %s", info.Slug, info.ID, app.Key.Fingerprint()), nil
}

func checkInstallation(ctx context.Context, c *Client, t *connectors.Type, app App) (string, error) {
	inst, err := c.GetInstallation(ctx, app)
	if err != nil {
		return "", err
	}
	if inst.AppID != 0 && inst.AppID != app.AppID {
		return "", fmt.Errorf("installation %d belongs to App %d, not %d", inst.ID, inst.AppID, app.AppID)
	}
	if inst.SuspendedAt != nil {
		return "", fmt.Errorf("installation %d on %s is suspended; unsuspend it in GitHub", inst.ID, inst.Account.Login)
	}
	var problems []string
	for _, p := range t.Spec.Auth.ForbiddenPermissions {
		if level, ok := inst.Permissions[p]; ok {
			problems = append(problems, fmt.Sprintf("remove the %s permission (it is %s)", p, level))
		}
	}
	for _, p := range sortedKeys(t.Spec.Auth.Permissions) {
		want, have := t.Spec.Auth.Permissions[p], inst.Permissions[p]
		if have == "" || (want == "write" && have != "write" && have != "admin") {
			problems = append(problems, fmt.Sprintf("set %s to %s (it is %s)", p, want, orNone(have)))
		}
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("the App's permissions do not match the setup guide: %s. After changing them, accept the new permissions on the installation", strings.Join(problems, "; "))
	}
	return fmt.Sprintf("installed on %s (%s repositories)", inst.Account.Login, inst.RepositorySelection), nil
}

func checkToken(ctx context.Context, c *Client, _ *connectors.Type, app App) (string, error) {
	tok, err := c.InstallationToken(ctx, app, TokenRequest{Permissions: map[string]string{"metadata": "read"}})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("minted a metadata-read installation token valid until %s", tok.ExpiresAt.UTC().Format(time.RFC3339)), nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func orNone(s string) string {
	if s == "" {
		return "not granted"
	}
	return s
}
