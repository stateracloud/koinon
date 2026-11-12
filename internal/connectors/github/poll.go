// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/codehost"
)

// Polling (ADR 0066, amendment of 2026-11-12). The poller in internal/triggers/poll owns the
// schedule; this file only answers one poll. Upstream references (github/docs @ dd7d394):
// best-practices-for-using-the-rest-api.md (conditional requests; a 304 to an authenticated
// conditional request does not count against the primary rate limit; x-poll-interval), activity/
// events.md (the repository events feed is meant for ETag polling, holds at most 300 events of the
// last 30 days) and rate-limits-for-the-rest-api.md (x-ratelimit-reset, retry-after).

// Poll pages and bounds.
const (
	pollPerPage = 100
	// pollMaxPages bounds the walk back to the cursor: the events feed serves at most 300 events
	// (3 pages of 100), and a comment burst larger than this between two polls is cut at the oldest.
	pollMaxPages = 3
	// maxItemPayload bounds the payload kept of one item; a larger one is delivered without it.
	maxItemPayload = 64 << 10
	// maxHeaderSeconds caps a seconds header (X-Poll-Interval, Retry-After) before it becomes a
	// Duration, so a huge value cannot overflow into a short one; the poller caps waits lower.
	maxHeaderSeconds = 7 * 24 * 3600
)

// ConditionalResponse is the answer to a conditional GET. A 304 is a result, not an error: it is
// the common answer of a poll and costs no primary rate limit.
type ConditionalResponse struct {
	// NotModified is true for a 304; Body is then empty and was not read.
	NotModified bool
	// Validator is the response's ETag, else its Last-Modified; on a 304 without either, the one
	// that was sent.
	Validator codehost.Validator
	// PollInterval is the response's X-Poll-Interval; zero when it has none.
	PollInterval time.Duration
	Body         []byte
}

// GetConditional sends GET path?query with If-None-Match or If-Modified-Since from v, always with
// the same query so the validator stays valid (F3 of the research). A non-2xx answer other than 304
// is an *APIError, wrapped in a *codehost.RateLimitError when GitHub says the token is rate
// limited. It never sleeps.
func (c *Client) GetConditional(ctx context.Context, tok Token, path string, query url.Values, v codehost.Validator) (ConditionalResponse, error) {
	if tok.value == "" {
		return ConditionalResponse{}, errors.New("github: no installation token")
	}
	full := c.baseURL + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return ConditionalResponse{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", APIVersion)
	req.Header.Set("Authorization", "Bearer "+tok.value)
	switch v.Kind {
	case codehost.ValidatorETag:
		req.Header.Set("If-None-Match", v.Value)
	case codehost.ValidatorLastModified:
		req.Header.Set("If-Modified-Since", v.Value)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ConditionalResponse{}, fmt.Errorf("github: GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := ConditionalResponse{PollInterval: pollInterval(resp.Header), Validator: validatorOf(resp.Header)}
	if resp.StatusCode == http.StatusNotModified {
		out.NotModified = true
		if out.Validator.Kind == codehost.ValidatorNone {
			out.Validator = v
		}
		return out, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return ConditionalResponse{}, fmt.Errorf("github: GET %s: read response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		apiErr := &APIError{Method: http.MethodGet, Path: path, Status: resp.StatusCode, Message: truncate(e.Message, 300)}
		if at, ok := rateLimitedUntil(resp, c.now()); ok {
			return ConditionalResponse{}, &codehost.RateLimitError{Err: apiErr, RetryAt: at}
		}
		return ConditionalResponse{}, apiErr
	}
	out.Body = data
	return out, nil
}

func validatorOf(h http.Header) codehost.Validator {
	if e := h.Get("ETag"); e != "" {
		return codehost.Validator{Kind: codehost.ValidatorETag, Value: e}
	}
	if lm := h.Get("Last-Modified"); lm != "" {
		return codehost.Validator{Kind: codehost.ValidatorLastModified, Value: lm}
	}
	return codehost.Validator{Kind: codehost.ValidatorNone}
}

func pollInterval(h http.Header) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(h.Get("X-Poll-Interval")))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(min(n, maxHeaderSeconds)) * time.Second
}

// rateLimitedUntil reads GitHub's rate-limit answer (F6): a primary limit is a 403 or 429 with
// x-ratelimit-remaining 0 and x-ratelimit-reset (Unix seconds); a secondary limit is a 403 or 429
// with retry-after (seconds), or without either header (then at least a minute). The caller caps
// and stores the time; nothing here sleeps.
func rateLimitedUntil(resp *http.Response, now time.Time) (time.Time, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return time.Time{}, false
	}
	h := resp.Header
	if s, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && s >= 0 {
		return now.Add(time.Duration(min(s, maxHeaderSeconds)) * time.Second), true
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return time.Unix(reset, 0), true
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return now.Add(time.Minute), true
	}
	return time.Time{}, false
}

// Poll implements codehost.Poller for the repository events feed and the repository's issue
// comments. The first page is conditional; a 304 returns unchanged before the body is read.
func (a *access) Poll(ctx context.Context, res codehost.Resource, st codehost.PollState) (codehost.PollResult, error) {
	var (
		path  string
		query = url.Values{"per_page": {strconv.Itoa(pollPerPage)}}
		item  func(json.RawMessage) (codehost.PollItem, string, error)
	)
	r := repo(res.Repo)
	switch res.Kind {
	case codehost.ResourceRepoEvents:
		path = r.path("/events")
		item = eventItem
	case codehost.ResourceIssueComments:
		path = r.path("/issues/comments")
		// A stable sort and identical parameters on every poll keep the ETag valid.
		query.Set("sort", "created")
		query.Set("direction", "desc")
		item = commentItem
	default:
		return codehost.PollResult{}, fmt.Errorf("%w: github %s", codehost.ErrResourceUnsupported, res.Kind)
	}
	sent := st.Validator
	if !st.Baselined {
		sent = codehost.Validator{Kind: codehost.ValidatorNone}
	}
	first, err := a.c.GetConditional(ctx, a.tok, path, query, sent)
	if err != nil {
		return codehost.PollResult{}, err
	}
	if first.NotModified {
		next := st
		next.Validator = first.Validator
		return codehost.PollResult{Changed: false, State: next, Interval: first.PollInterval}, nil
	}
	walk := pageWalk{cursor: st.Cursor, item: item}
	done, err := walk.page(first.Body)
	for p := 2; err == nil && !done && st.Baselined && p <= pollMaxPages; p++ {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("page", strconv.Itoa(p))
		var more ConditionalResponse
		more, err = a.c.GetConditional(ctx, a.tok, path, q, codehost.Validator{Kind: codehost.ValidatorNone})
		if err == nil {
			done, err = walk.page(more.Body)
		}
	}
	if err != nil {
		return codehost.PollResult{}, err
	}
	next := codehost.PollState{Validator: first.Validator, Cursor: walk.high, Baselined: true}
	if next.Cursor == "" {
		next.Cursor = st.Cursor
	}
	out := codehost.PollResult{Changed: true, State: next, Interval: first.PollInterval}
	if st.Baselined {
		out.Items = walk.items()
	}
	return out, nil
}

// pageWalk collects the items newer than cursor from pages served newest first.
type pageWalk struct {
	cursor string
	item   func(json.RawMessage) (codehost.PollItem, string, error)
	high   string
	newest []codehost.PollItem // newest first
}

// page adds one page; done is true once it reached the cursor or the list ended.
func (w *pageWalk) page(body []byte) (done bool, _ error) {
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return true, fmt.Errorf("github: decode poll page: %w", err)
	}
	for _, m := range raw {
		it, id, err := w.item(m)
		if err != nil {
			return true, err
		}
		if w.high == "" || idLess(w.high, id) {
			w.high = id
		}
		if w.cursor != "" && !idLess(w.cursor, id) {
			return true, nil
		}
		w.newest = append(w.newest, it)
	}
	return len(raw) < pollPerPage, nil
}

// items returns the collected items oldest first.
func (w *pageWalk) items() []codehost.PollItem {
	out := make([]codehost.PollItem, 0, len(w.newest))
	for i := len(w.newest) - 1; i >= 0; i-- {
		out = append(out, w.newest[i])
	}
	return out
}

// idLess compares two decimal IDs without parsing them (GitHub IDs outgrow 32 bits).
func idLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func payload(m json.RawMessage) []byte {
	if len(m) > maxItemPayload {
		return nil
	}
	return append([]byte(nil), m...)
}

func eventItem(m json.RawMessage) (codehost.PollItem, string, error) {
	var e struct {
		ID        string    `json:"id"`
		Type      string    `json:"type"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(m, &e); err != nil || e.ID == "" || strings.Trim(e.ID, "0123456789") != "" {
		return codehost.PollItem{}, "", fmt.Errorf("github: event without a numeric id")
	}
	return codehost.PollItem{Key: "event:" + e.ID, Kind: e.Type, At: e.CreatedAt, Payload: payload(m)}, e.ID, nil
}

func commentItem(m json.RawMessage) (codehost.PollItem, string, error) {
	var c struct {
		ID        int64     `json:"id"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(m, &c); err != nil || c.ID <= 0 {
		return codehost.PollItem{}, "", fmt.Errorf("github: comment without an id")
	}
	id := strconv.FormatInt(c.ID, 10)
	return codehost.PollItem{Key: "comment:" + id, Kind: "issue_comment", At: c.CreatedAt, Payload: payload(m)}, id, nil
}

var _ codehost.Poller = (*access)(nil)
