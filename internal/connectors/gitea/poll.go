// SPDX-License-Identifier: Apache-2.0

package gitea

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/codehost"
)

// Polling (ADR 0066, amendment of 2026-11-12). Gitea's API lists carry no ETag and no
// Last-Modified (go-gitea/gitea @ 37fa67f, modules/httpcache: API responses are
// "max-age=0, private, must-revalidate"; the ETag helpers serve downloads only) and no poll-interval
// or rate-limit header, so a poll reads the first page and compares its hash with the stored one:
// an identical page is "unchanged" and ends the poll before any trigger work. The issue-comment list
// takes `since` (services/context/utils.go), which keeps the page small and the hash stable while
// nothing is new.

const (
	// pollOverlap is how far before the cursor's time `since` reaches, so a comment the server
	// dated slightly before one Koinon already saw (clock skew between the poll and the write) is
	// not skipped; the ID cursor drops the ones already delivered.
	pollOverlap = 5 * time.Minute
	// pollMaxPages bounds one poll's walk (pages of maxPerPage, oldest first).
	pollMaxPages = 5
	// maxItemPayload bounds the payload kept of one item.
	maxItemPayload = 64 << 10
)

// commentCursor is the issue_comments cursor: the highest comment ID delivered and the creation
// time `since` is computed from, "<id>@<unix seconds>".
type commentCursor struct {
	id int64
	at time.Time
}

func parseCommentCursor(s string) (commentCursor, error) {
	id, at, ok := strings.Cut(s, "@")
	n, err1 := strconv.ParseInt(id, 10, 64)
	u, err2 := strconv.ParseInt(at, 10, 64)
	if !ok || err1 != nil || err2 != nil {
		return commentCursor{}, fmt.Errorf("gitea: bad poll cursor %q", s)
	}
	return commentCursor{id: n, at: time.Unix(u, 0).UTC()}, nil
}

func (c commentCursor) String() string {
	return strconv.FormatInt(c.id, 10) + "@" + strconv.FormatInt(c.at.Unix(), 10)
}

// Poll implements codehost.Poller for a repository's issue comments.
func (a *access) Poll(ctx context.Context, res codehost.Resource, st codehost.PollState) (codehost.PollResult, error) {
	if res.Kind != codehost.ResourceIssueComments {
		return codehost.PollResult{}, fmt.Errorf("%w: gitea %s", codehost.ErrResourceUnsupported, res.Kind)
	}
	cur := commentCursor{at: a.now().UTC().Truncate(time.Second)}
	if st.Baselined {
		var err error
		if cur, err = parseCommentCursor(st.Cursor); err != nil {
			return codehost.PollResult{}, err
		}
	}
	since := cur.at.Add(-pollOverlap)
	query := func(page int) url.Values {
		return url.Values{
			"since": {since.Format(time.RFC3339)},
			"page":  {strconv.Itoa(page)}, "limit": {strconv.Itoa(maxPerPage)},
		}
	}
	path := escapeRepo(res.Repo) + "/issues/comments"
	var raw []json.RawMessage
	if _, err := a.c.do(ctx, "GET", path, query(1), nil, &raw); err != nil {
		return codehost.PollResult{}, err
	}
	h := sha256.New()
	for _, m := range raw {
		h.Write(m)
		h.Write([]byte{'\n'})
	}
	hash := codehost.Validator{Kind: codehost.ValidatorPageHash, Value: hex.EncodeToString(h.Sum(nil))}
	if st.Baselined && st.Validator == hash {
		return codehost.PollResult{Changed: false, State: st}, nil
	}
	next := cur
	var items []codehost.PollItem
	for p := 1; ; p++ {
		for _, m := range raw {
			var c struct {
				ID      int64     `json:"id"`
				Created time.Time `json:"created_at"`
			}
			if err := json.Unmarshal(m, &c); err != nil || c.ID <= 0 {
				return codehost.PollResult{}, fmt.Errorf("gitea: comment without an id")
			}
			if c.ID <= cur.id {
				continue
			}
			if c.ID > next.id {
				next.id = c.ID
			}
			if c.Created.After(next.at) {
				next.at = c.Created.UTC().Truncate(time.Second)
			}
			var body []byte
			if len(m) <= maxItemPayload {
				body = append([]byte(nil), m...)
			}
			items = append(items, codehost.PollItem{Key: "comment:" + strconv.FormatInt(c.ID, 10), Kind: "issue_comment", At: c.Created, Payload: body})
		}
		if len(raw) < maxPerPage || p >= pollMaxPages {
			break
		}
		raw = nil
		if _, err := a.c.do(ctx, "GET", path, query(p+1), nil, &raw); err != nil {
			return codehost.PollResult{}, err
		}
	}
	out := codehost.PollResult{Changed: true, State: codehost.PollState{Validator: hash, Cursor: next.String(), Baselined: true}}
	if st.Baselined {
		out.Items = items
	}
	return out, nil
}

var _ codehost.Poller = (*access)(nil)
