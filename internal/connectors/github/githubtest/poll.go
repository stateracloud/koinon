// SPDX-License-Identifier: Apache-2.0

package githubtest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// pollFeed is what the fake serves to pollers for one repository: the events feed and the
// repository-wide issue comments, each newest first, with an ETag and an optional X-Poll-Interval.
type pollFeed struct {
	events   []map[string]any // oldest first
	comments []map[string]any // oldest first
	interval int              // X-Poll-Interval in seconds; 0 sends none
	// retryAfter, when set, answers the next poll with 403 and Retry-After (a secondary limit).
	retryAfter int
	gets       int // GETs of either list, conditional or not
	notMod     int // of which answered 304
}

// pollIDs numbers events and comments; IDs outgrow 32 bits, as GitHub's do.
var pollIDs atomic.Int64

func nextPollID() int64 { return 40_000_000_000 + pollIDs.Add(1) }

func (f *Fake) feed(name string) *pollFeed {
	if f.polls == nil {
		f.polls = map[string]*pollFeed{}
	}
	p, ok := f.polls[name]
	if !ok {
		p = &pollFeed{}
		f.polls[name] = p
	}
	return p
}

// AddEvent appends an event of type typ (for example PullRequestEvent) to a repository's events
// feed and returns its ID.
func (f *Fake) AddEvent(repo, typ string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := strconv.FormatInt(nextPollID(), 10)
	p := f.feed(repo)
	p.events = append(p.events, map[string]any{
		"id": id, "type": typ, "public": false, "created_at": f.Now().UTC().Format(time.RFC3339),
		"repo":    map[string]any{"name": f.Account + "/" + repo},
		"payload": map[string]any{},
	})
	return id
}

// AddRepoComment appends a comment to a repository's issue comments and returns its ID.
func (f *Fake) AddRepoComment(repo string, issue int64, body string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := nextPollID()
	p := f.feed(repo)
	p.comments = append(p.comments, map[string]any{
		"id": id, "body": body, "created_at": f.Now().UTC().Format(time.RFC3339),
		"issue_url": f.Server.URL + "/repos/" + f.Account + "/" + repo + "/issues/" + strconv.FormatInt(issue, 10),
	})
	return id
}

// SetPollInterval makes the repository's poll answers carry X-Poll-Interval: seconds (0: none).
func (f *Fake) SetPollInterval(repo string, seconds int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.feed(repo).interval = seconds
}

// RateLimitNextPoll answers the next poll of repo with a secondary rate limit (403, Retry-After).
func (f *Fake) RateLimitNextPoll(repo string, retryAfter int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.feed(repo).retryAfter = retryAfter
}

// PollGets returns how many GETs of repo's polled lists the fake served, and how many of them it
// answered 304.
func (f *Fake) PollGets(repo string) (gets, notModified int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.feed(repo)
	return p.gets, p.notMod
}

func (f *Fake) routePolls(mux *http.ServeMux) {
	mux.HandleFunc("GET /repos/{owner}/{repo}/events", f.withRepo("metadata", "read", func(w http.ResponseWriter, r *http.Request, rp *Repo) {
		f.servePoll(w, r, rp.Name, func(p *pollFeed) []map[string]any { return p.events })
	}))
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/comments", f.withRepo("issues", "read", func(w http.ResponseWriter, r *http.Request, rp *Repo) {
		f.servePoll(w, r, rp.Name, func(p *pollFeed) []map[string]any { return p.comments })
	}))
}

// servePoll serves one page of a list newest first with GitHub's conditional-request behaviour:
// the ETag is a hash of the page, and If-None-Match with the current ETag answers 304 without a
// body. f.mu is held (withRepo).
func (f *Fake) servePoll(w http.ResponseWriter, r *http.Request, repo string, list func(*pollFeed) []map[string]any) {
	p := f.feed(repo)
	p.gets++
	if p.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(p.retryAfter))
		p.retryAfter = 0
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "You have exceeded a secondary rate limit."})
		return
	}
	all := list(p)
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage < 1 || perPage > 100 {
		perPage = 30
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	out := []map[string]any{}
	for i := len(all) - 1 - (page-1)*perPage; i >= 0 && len(out) < perPage; i-- {
		out = append(out, all[i])
	}
	body, err := json.Marshal(out)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	sum := sha256.Sum256(append([]byte(r.URL.RawQuery+"\n"), body...))
	etag := `W/"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("ETag", etag)
	if p.interval > 0 {
		w.Header().Set("X-Poll-Interval", strconv.Itoa(p.interval))
	}
	if r.Header.Get("If-None-Match") == etag {
		p.notMod++
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
