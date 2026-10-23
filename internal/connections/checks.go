// SPDX-License-Identifier: Apache-2.0

package connections

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/connectors/github"
)

// CheckResult is one setup-guide check's outcome. Message never contains
// secrets.
type CheckResult struct {
	Check   string `json:"check"`
	Step    string `json:"step"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// checkFunc runs one built-in check (ADR 0021: referenced by name from the
// setup guide, implemented in Go).
type checkFunc func(ctx context.Context, gh *github.Client, t *connectors.Type, app github.App) (string, error)

var githubChecks = map[string]checkFunc{
	"github.app":          checkGitHubApp,
	"github.installation": checkGitHubInstallation,
	"github.token":        checkGitHubToken,
}

// GitHubCheckNames are the checks this package implements, so a test can
// assert that every check the setup guide names exists.
func GitHubCheckNames() []string {
	names := make([]string, 0, len(githubChecks))
	for n := range githubChecks {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// TestGitHub runs the GitHub setup guide's checks in step order. After the
// first failure the remaining checks are reported as skipped.
func TestGitHub(ctx context.Context, gh *github.Client, t *connectors.Type, app github.App) []CheckResult {
	var out []CheckResult
	failed := false
	for _, step := range t.Spec.SetupGuide.Steps {
		if step.Check == "" {
			continue
		}
		r := CheckResult{Check: step.Check, Step: step.ID}
		fn, ok := githubChecks[step.Check]
		switch {
		case !ok:
			r.Message = "Koinon does not implement this check"
			failed = true
		case failed:
			r.Message = "skipped: an earlier check failed"
		default:
			msg, err := fn(ctx, gh, t, app)
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
	var api *github.APIError
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

func checkGitHubApp(ctx context.Context, gh *github.Client, _ *connectors.Type, app github.App) (string, error) {
	info, err := gh.GetApp(ctx, app)
	if err != nil {
		return "", err
	}
	if info.ID != app.AppID {
		return "", fmt.Errorf("GitHub returned App %d, not %d", info.ID, app.AppID)
	}
	return fmt.Sprintf("authenticated as the App %q (ID %d) with the key %s", info.Slug, info.ID, app.Key.Fingerprint()), nil
}

func checkGitHubInstallation(ctx context.Context, gh *github.Client, t *connectors.Type, app github.App) (string, error) {
	inst, err := gh.GetInstallation(ctx, app)
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

func checkGitHubToken(ctx context.Context, gh *github.Client, _ *connectors.Type, app github.App) (string, error) {
	tok, err := gh.InstallationToken(ctx, app, github.TokenRequest{Permissions: map[string]string{"metadata": "read"}})
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
