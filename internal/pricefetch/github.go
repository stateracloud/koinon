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

// commitWindow is how many of the file's newest commits the adapter looks at.
const commitWindow = 30

// pinnedCommit returns the commit to read the file at: the newest commit C of the default branch
// that touches the file such that C is at least CommitCooldown old and the next commit that touched
// the file came at least CommitCooldown after C (or there is none). The second condition is what
// makes a commit that is reverted or replaced within a day never reach a proposal: the bad commit
// X is skipped for good, because its successor, the revert, followed it within a day, and the
// commit before X is pinned instead. The dates are checked here and not left to the server's
// `until`, which it may ignore.
func (g githubFile) pinnedCommit(ctx context.Context, now time.Time) (sha string, at time.Time, err error) {
	q := url.Values{}
	q.Set("path", g.path)
	q.Set("until", now.UTC().Format(time.RFC3339))
	q.Set("per_page", fmt.Sprint(commitWindow))
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
	for _, c := range commits {
		if !shaRE.MatchString(c.SHA) {
			return "", time.Time{}, fmt.Errorf("pricefetch: %s: %q is not a commit SHA", g.name, c.SHA)
		}
		if c.Commit.Committer.Date.IsZero() {
			return "", time.Time{}, fmt.Errorf("pricefetch: %s: commit %s has no date", g.name, c.SHA)
		}
	}
	cutoff := now.Add(-CommitCooldown)
	for i, c := range commits { // newest first
		d := c.Commit.Committer.Date.UTC()
		if d.After(cutoff) {
			continue
		}
		if i > 0 && commits[i-1].Commit.Committer.Date.UTC().Sub(d) < CommitCooldown {
			continue // replaced within a day: not stable
		}
		return c.SHA, d, nil
	}
	return "", time.Time{}, fmt.Errorf("pricefetch: %s: no commit of the file is both %s old and unchanged for %[3]s after it among the newest %d: %w", g.name, CommitCooldown, CommitCooldown, len(commits), ErrNoOldCommit)
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
