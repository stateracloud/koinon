// SPDX-License-Identifier: Apache-2.0

package pricefetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// The pinned hosts of the GitHub-hosted sources (ADR 0098 decision 2.3).
const (
	githubAPIHost = "api.github.com"
	githubRawHost = "raw.githubusercontent.com"
)

// CommitCooldown is how old a commit must be before an adapter reads a file at it (ADR 0098
// decision 2.4, ADR 0079's cooldown applied to data).
const CommitCooldown = 24 * time.Hour

// ErrNoOldCommit is returned when the newest commit of the file is younger than CommitCooldown: the
// source is in its cooldown and proposes nothing yet.
var ErrNoOldCommit = errors.New("pricefetch: no commit of the file is old enough")

// githubFile is one file of one repository, read at a pinned commit.
type githubFile struct {
	client *http.Client
	repo   string // owner/name
	path   string // the file in the repository
	// api and raw are the base URLs of the two hosts; tests point them at httptest servers.
	api, raw       string
	apiPin, rawPin pin
	name           string // the source, for errors
}

func newGithubFile(client *http.Client, name, repo, path string) githubFile {
	return githubFile{
		client: client, repo: repo, path: path, name: name,
		api: "https://" + githubAPIHost, raw: "https://" + githubRawHost,
		apiPin: pin{host: githubAPIHost}, rawPin: pin{host: githubRawHost},
	}
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

type ghCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

// pinnedCommit returns the newest commit of the default branch that touches the file and is at
// least CommitCooldown older than now. GitHub filters by `until` too, but a server that ignores it,
// or a clock that differs, must not weaken the rule, so the date is checked here as well.
func (g githubFile) pinnedCommit(ctx context.Context, now time.Time) (sha string, at time.Time, err error) {
	q := url.Values{}
	q.Set("path", g.path)
	q.Set("until", now.Add(-CommitCooldown).UTC().Format(time.RFC3339))
	q.Set("per_page", "1")
	body, _, err := httpGet(ctx, g.client, g.api+"/repos/"+g.repo+"/commits?"+q.Encode(), g.apiPin, g.name, "application/vnd.github+json")
	if err != nil {
		return "", time.Time{}, err
	}
	var commits []ghCommit
	if err := json.Unmarshal(body, &commits); err != nil {
		return "", time.Time{}, fmt.Errorf("pricefetch: %s: commits: %w", g.name, err)
	}
	if len(commits) == 0 {
		return "", time.Time{}, fmt.Errorf("pricefetch: %s: the commit list is empty: %w", g.name, ErrNoOldCommit)
	}
	c := commits[0]
	if !shaRE.MatchString(c.SHA) {
		return "", time.Time{}, fmt.Errorf("pricefetch: %s: %q is not a commit SHA", g.name, c.SHA)
	}
	at = c.Commit.Committer.Date.UTC()
	if at.IsZero() {
		return "", time.Time{}, fmt.Errorf("pricefetch: %s: commit %s has no date", g.name, c.SHA)
	}
	if at.After(now.Add(-CommitCooldown)) {
		return "", time.Time{}, fmt.Errorf("pricefetch: %s: the newest commit %s is %s old, under the %s cooldown: %w", g.name, c.SHA, now.Sub(at).Round(time.Second), CommitCooldown, ErrNoOldCommit)
	}
	return c.SHA, at, nil
}

// fetch reads the file at the pinned commit and returns the body with the commit's SHA, its date
// and the URL it was read from. It never reads a branch head.
func (g githubFile) fetch(ctx context.Context, now time.Time) (body []byte, sha string, at time.Time, rawURL string, err error) {
	sha, at, err = g.pinnedCommit(ctx, now)
	if err != nil {
		return nil, "", time.Time{}, "", err
	}
	rawURL = g.raw + "/" + g.repo + "/" + sha + "/" + strings.TrimPrefix(g.path, "/")
	body, _, err = httpGet(ctx, g.client, rawURL, g.rawPin, g.name, "application/json")
	return body, sha, at, rawURL, err
}
