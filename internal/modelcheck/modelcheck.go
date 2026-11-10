// SPDX-License-Identifier: Apache-2.0

// Package modelcheck is the provider echo check (rc.6): one tiny chat request, through the gateway,
// to one model of a provider, asking it to repeat a fixed line exactly. If the reply differs, the
// provider (or an account setting at the provider, such as OpenRouter's masking of personal data)
// changed the text on the way, which silently corrupts agent work: names, paths and namespaces in
// prompts and tool calls would no longer be the ones the agent meant.
//
// The check runs only when an admin asks for it, from the Providers page or the admin API. It is
// never scheduled and CI never runs it against a real model: tests point it at the fake model
// server. The request goes through the gateway like an agent's, so it is priced and metered like any
// request. The latest result of each provider is kept (provider_echo_checks) and shown with its time.
package modelcheck

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/restorehold"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/agentmodel"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/outward"
	"github.com/stateracloud/koinon/internal/prices"
)

// Line is the fixed test line. It has a person's name, an address-like string and a Kubernetes
// namespace: what a data-masking filter looks for, and what an agent's prompts carry.
const Line = "Maria Schneider, Hauptstrasse 12, 10115 Berlin, namespace payments-prod-eu"

// Prompt is the one message the check sends.
const Prompt = "Echo this line back exactly, character for character, nothing else: " + Line

// Chat sends a single user message to a model of the org through the gateway and returns the text
// of the reply. format is the model's wire format (models.format, the one the run uses, ADR 0069):
// the request goes to the route and has the shape of that format.
type Chat interface {
	Complete(ctx context.Context, org pgtype.UUID, model string, format modelformat.Format, prompt string) (string, error)
}

// ErrNotFound means the provider does not exist in the org.
var ErrNotFound = errors.New("not found")

// InvalidError is a refused request; the message is safe to show.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// UnavailableError means the check could not run or the gateway did not answer it.
type UnavailableError struct{ Msg string }

func (e *UnavailableError) Error() string { return e.Msg }

// BusyError means a check of the org is already running.
type BusyError struct{}

func (*BusyError) Error() string { return "a check is already running; try again when it has finished" }

// Op is the kind of a diff segment.
type Op string

// Diff operations.
const (
	Equal    Op = "equal"
	Deleted  Op = "deleted"
	Inserted Op = "inserted"
)

// Segment is a run of characters of the line sent, the reply, or both.
type Segment struct {
	Op   Op
	Text string
}

// Result is the outcome of an echo check.
type Result struct {
	ProviderID pgtype.UUID
	Model      string
	Unchanged  bool
	Sent       string
	Received   string
	Diff       []Segment
	CheckedAt  time.Time
}

// Service runs echo checks.
type Service struct {
	pool   *pgxpool.Pool
	chat   Chat
	prices prices.Resolver
	// Timeout bounds the request to the gateway.
	Timeout time.Duration
	now     func() time.Time
	hold    *restorehold.Guard
}

// WithRestoreHold makes Check refuse with restorehold.ErrHeld while g holds (issue #462, ADR 0107).
func (s *Service) WithRestoreHold(g *restorehold.Guard) *Service {
	s.hold = g
	return s
}

// New returns a Service. A nil chat makes every check fail as unavailable.
func New(pool *pgxpool.Pool, chat Chat, rv prices.Resolver) *Service {
	return &Service{pool: pool, chat: chat, prices: rv, Timeout: 60 * time.Second, now: time.Now}
}

// Compare says whether reply is the line, and the difference otherwise. Whitespace around the reply
// and one pair of quotes or backticks wrapped around the whole reply are ignored: a model that
// quotes its answer has not been masked. Everything else must match character for character.
func Compare(sent, reply string) (bool, []Segment) {
	got := normalize(reply)
	diff := Diff(sent, got)
	return got == sent, diff
}

func normalize(s string) string {
	s = strings.TrimSpace(s)
	for _, q := range []string{"\"", "`", "'"} {
		if len(s) >= 2*len(q) && strings.HasPrefix(s, q) && strings.HasSuffix(s, q) && !strings.Contains(s[1:len(s)-1], q) {
			return strings.TrimSpace(s[len(q) : len(s)-len(q)])
		}
	}
	return s
}

// tokenPattern splits text into words (letters, digits, hyphens and underscores) and single other
// characters, so a diff replaces whole words rather than interleaving stray matching letters.
var tokenPattern = regexp.MustCompile(`[\p{L}\p{N}_-]+|(?s:.)`)

// Diff is the difference from a to b by word, as the longest common subsequence of their tokens
// gives it, with adjacent segments of one kind joined.
func Diff(a, b string) []Segment {
	x, y := tokenPattern.FindAllString(a, -1), tokenPattern.FindAllString(b, -1)
	// lcs[i][j] is the length of the longest common subsequence of x[i:] and y[j:].
	lcs := make([][]int, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []Segment
	add := func(op Op, t string) {
		if n := len(out); n > 0 && out[n-1].Op == op {
			out[n-1].Text += t
			return
		}
		out = append(out, Segment{Op: op, Text: t})
	}
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			add(Equal, x[i])
			i++
			j++
		case i < len(x) && (j == len(y) || lcs[i+1][j] >= lcs[i][j+1]):
			// What was removed is listed before what replaced it.
			add(Deleted, x[i])
			i++
		default:
			add(Inserted, y[j])
			j++
		}
	}
	return out
}

// Check sends the echo request to a model of the provider and stores the result. The model must
// belong to the provider and be served and priced by the org (the gateway answers 404 for the
// rest). The check, its stored result and its audit event are the only effects; a gateway failure
// stores nothing and returns *UnavailableError.
func (s *Service) Check(ctx context.Context, org, provider pgtype.UUID, model string, actor audit.Actor, user pgtype.UUID) (Result, error) {
	if err := s.hold.Err(); err != nil {
		return Result{}, err
	}
	q := store.New(s.pool)
	p, err := q.GetProvider(ctx, store.GetProviderParams{OrgID: org, ID: provider})
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, ErrNotFound
	} else if err != nil {
		return Result{}, err
	}
	if err := outward.Check(p.AdminDisabledAt); err != nil {
		return Result{}, err
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return Result{}, &InvalidError{Msg: "a model is required"}
	}
	served, err := q.ListServedCatalog(ctx, org)
	if err != nil {
		return Result{}, err
	}
	onProvider := false
	format := modelformat.OpenAI
	for _, m := range served {
		if m.ProviderID == p.ID && m.ModelName == model {
			onProvider = true
			format = modelformat.Format(m.ModelFormat)
		}
	}
	if !onProvider {
		return Result{}, &InvalidError{Msg: fmt.Sprintf("the model %q is not served by the provider %q", model, p.Name)}
	}
	var unusable *agentmodel.UnusableError
	if err := agentmodel.Check(ctx, s.prices, q, org, model, s.now()); errors.As(err, &unusable) {
		return Result{}, &InvalidError{Msg: unusable.Error() + "; the check needs a model the gateway serves"}
	} else if err != nil {
		return Result{}, err
	}
	if s.chat == nil {
		return Result{}, &UnavailableError{Msg: "the echo check is not configured: koinon-api has no gateway to send it through"}
	}
	// One check in flight per org (ADR 0041 amendment): the lock is held, in the transaction that
	// stores the result, for as long as the request takes.
	var res Result
	_, err = audit.InPgTx(ctx, s.pool, func(tx pgx.Tx, b audit.Batch) error {
		var got bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('modelcheck:' || $1::text, 0))`, org.String()).Scan(&got); err != nil {
			return err
		}
		if !got {
			return &BusyError{}
		}
		cctx, cancel := context.WithTimeout(ctx, s.Timeout)
		defer cancel()
		reply, err := s.chat.Complete(cctx, org, model, format, Prompt)
		if err != nil {
			return &UnavailableError{Msg: "the model did not answer the check: " + err.Error()}
		}
		unchanged, diff := Compare(Line, reply)
		res = Result{ProviderID: p.ID, Model: model, Unchanged: unchanged, Sent: Line, Received: reply, Diff: diff}
		tq := store.New(tx)
		row, err := tq.UpsertProviderEchoCheck(ctx, store.UpsertProviderEchoCheckParams{
			OrgID: org, ProviderID: p.ID, Model: model, Unchanged: unchanged, Sent: Line, Received: reply,
			CheckedBy: user,
		})
		if err != nil {
			return err
		}
		res.CheckedAt = row.CheckedAt.Time
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "provider.echo_check", TargetKind: "provider", TargetID: p.ID.String(),
			After: map[string]any{"model": model, "unchanged": unchanged},
		})
		return nil
	})
	return res, err
}

// Latest returns the latest result of each provider that has one, by provider ID.
func (s *Service) Latest(ctx context.Context, org pgtype.UUID) (map[pgtype.UUID]Result, error) {
	rows, err := store.New(s.pool).ListProviderEchoChecks(ctx, org)
	if err != nil {
		return nil, err
	}
	out := make(map[pgtype.UUID]Result, len(rows))
	for _, r := range rows {
		out[r.ProviderID] = FromRow(r)
	}
	return out, nil
}

// FromRow converts a stored result; the diff is recomputed from what was sent and received.
func FromRow(r store.ProviderEchoCheck) Result {
	_, diff := Compare(r.Sent, r.Received)
	return Result{
		ProviderID: r.ProviderID, Model: r.Model, Unchanged: r.Unchanged, Sent: r.Sent, Received: r.Received,
		Diff: diff, CheckedAt: r.CheckedAt.Time,
	}
}
