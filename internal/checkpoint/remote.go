// SPDX-License-Identifier: Apache-2.0

package checkpoint

// How koinon-agentd gets an agent's repository password without holding a key-encryption key (ADR
// 0054: a compromised agentd cannot open any org's secrets). koinon-api keeps the keys; the
// password Server creates or opens the password and answers agentd per checkpoint, over the
// saved-work listener only koinon-agentd may reach. The request is authenticated with an HMAC
// made with the same key as the saved-work posts, over a domain-separated message, good for
// one agent for a minute. The password is released to the writer only, and is never logged.
//
// A request names what it wants (ADR 0085 decision 13): current=1 is the agent's current
// repository, which a backup writes to, and epoch=N is that repository epoch's, which a restore
// asks for. The credential covers the agent, whichever of its repositories is asked for, so the
// token is the same as before epochs existed. During a rolling upgrade either side may be older:
// - an older koinon-api ignores both parameters and answers with epoch 0, its only one; the client
//   checks the epoch of the reply, so asking it for epoch N>0 fails, and current gets epoch 0;
// - an older koinon-agentd sends neither and does not record the epoch it wrote to, so it is
//   answered only while the agent's current repository is epoch 0. Once a later epoch exists it
//   gets 409 Conflict and its backup fails loudly, instead of writing a checkpoint to epoch N that
//   its completion would record as epoch 0.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

const (
	// RepositoryPath is where the Server listens (on the saved-work listener).
	RepositoryPath = "/internal/home-repository"
	// RepositoryTokenLifetime is how long a request's credential is good for.
	RepositoryTokenLifetime = time.Minute
)

// RepositoryToken is the bearer credential for one request: an HMAC over the org, the agent and
// the expiry, with a prefix that keeps it from being a saved-work credential.
func RepositoryToken(key []byte, org, agent string, exp int64) string {
	m := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(m, "home-repository\n%s\n%s\n%d", org, agent, exp)
	return hex.EncodeToString(m.Sum(nil))
}

// ValidRepositoryToken reports whether token matches and exp is neither past nor too far away.
func ValidRepositoryToken(key []byte, token, org, agent string, exp int64, now time.Time) bool {
	if exp < now.Unix() || exp > now.Add(RepositoryTokenLifetime).Unix() {
		return false
	}
	return hmac.Equal([]byte(token), []byte(RepositoryToken(key, org, agent, exp)))
}

type repositoryReply struct {
	Epoch    int32  `json:"epoch"`
	Prefix   string `json:"prefix"`
	Password string `json:"password"`
}

// Server answers RepositoryPath in koinon-api, which holds the key-encryption keys.
type Server struct {
	Passwords Passwords
	Key       func(context.Context) ([]byte, error)
	Log       *slog.Logger
	Now       func() time.Time
}

// ServeHTTP implements http.Handler.
func (s Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log, now := s.Log, s.Now
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	var org, agent pgtype.UUID
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if org.Scan(q.Get("org")) != nil || agent.Scan(q.Get("agent")) != nil || !org.Valid || !agent.Valid || err != nil {
		http.Error(w, "bad parameters", http.StatusBadRequest)
		return
	}
	epoch, atEpoch, current := int32(0), q.Has("epoch"), q.Get("current") == "1"
	if atEpoch && current {
		http.Error(w, "bad parameters", http.StatusBadRequest)
		return
	}
	if atEpoch {
		n, err := strconv.ParseInt(q.Get("epoch"), 10, 32)
		if err != nil || n < 0 {
			http.Error(w, "bad parameters", http.StatusBadRequest)
			return
		}
		epoch = int32(n)
	}
	key, err := s.Key(r.Context())
	if err != nil {
		log.Error("home repository: reading the key failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ValidRepositoryToken(key, token, org.String(), agent.String(), exp, now()) {
		http.Error(w, "bad credential", http.StatusUnauthorized)
		return
	}
	var repo Repository
	if atEpoch {
		repo, err = s.Passwords.At(r.Context(), org, agent, epoch)
	} else {
		repo, err = s.Passwords.Current(r.Context(), org, agent)
		if err == nil && !current && repo.Epoch != 0 {
			log.Warn("home repository: an older koinon-agentd asked for a repository after a key change; refused until it is upgraded",
				"org", org.String(), "agent", agent.String(), "epoch", repo.Epoch)
			http.Error(w, "the agent's repository changed epoch; upgrade koinon-agentd", http.StatusConflict)
			return
		}
	}
	if errors.Is(err, ErrNoRepository) {
		http.Error(w, "no such repository", http.StatusNotFound)
		return
	}
	if errors.Is(err, ErrRepositoryLost) {
		http.Error(w, "the repository's password was lost", http.StatusGone)
		return
	}
	if err != nil {
		log.Error("home repository: opening the password failed", "org", org.String(), "agent", agent.String(), "epoch", epoch, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(repositoryReply(repo)) //nolint:gosec // the password is the reply: released to the writer only
}

// Remote is the agentd side: it asks koinon-api for each repository, per checkpoint, and keeps
// nothing. It satisfies agentd.Repositories.
type Remote struct {
	HTTP    *http.Client
	BaseURL string
	Key     func() ([]byte, error)
	Now     func() time.Time
}

// Get returns the agent's current repository.
func (c Remote) Get(ctx context.Context, org, agent pgtype.UUID) (Repository, error) {
	return c.ask(ctx, org, agent, nil)
}

// At returns the agent's repository of the given epoch.
func (c Remote) At(ctx context.Context, org, agent pgtype.UUID, epoch int32) (Repository, error) {
	return c.ask(ctx, org, agent, &epoch)
}

func (c Remote) ask(ctx context.Context, org, agent pgtype.UUID, epoch *int32) (Repository, error) {
	key, err := c.Key()
	if err != nil || len(key) == 0 {
		return Repository{}, errors.New("the key that signs repository requests is unreadable")
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	exp := now().Add(RepositoryTokenLifetime / 2).Unix()
	q := url.Values{"org": {org.String()}, "agent": {agent.String()}, "exp": {strconv.FormatInt(exp, 10)}}
	if epoch != nil {
		q.Set("epoch", strconv.FormatInt(int64(*epoch), 10))
	} else {
		q.Set("current", "1")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.BaseURL, "/")+RepositoryPath+"?"+q.Encode(), nil)
	if err != nil {
		return Repository{}, err
	}
	req.Header.Set("Authorization", "Bearer "+RepositoryToken(key, org.String(), agent.String(), exp))
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(req)
	if err != nil {
		return Repository{}, fmt.Errorf("asking koinon-api for the repository of agent %s: %w", agent, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
	if res.StatusCode == http.StatusNotFound && epoch != nil {
		return Repository{}, fmt.Errorf("%w: agent %s, epoch %d", ErrNoRepository, agent, *epoch)
	}
	if res.StatusCode == http.StatusGone {
		return Repository{}, fmt.Errorf("%w: agent %s", ErrRepositoryLost, agent)
	}
	if res.StatusCode != http.StatusOK {
		return Repository{}, fmt.Errorf("koinon-api answered %d for the repository of agent %s", res.StatusCode, agent)
	}
	var out repositoryReply
	if err := json.Unmarshal(body, &out); err != nil || out.Prefix == "" || out.Password == "" {
		return Repository{}, errors.New("koinon-api answered with an unreadable repository")
	}
	if epoch != nil && out.Epoch != *epoch {
		// An older koinon-api ignores the parameter and answers with epoch 0.
		return Repository{}, fmt.Errorf("koinon-api answered with repository epoch %d of agent %s, not %d; is it older than this koinon-agentd?", out.Epoch, agent, *epoch)
	}
	return Repository(out), nil
}
