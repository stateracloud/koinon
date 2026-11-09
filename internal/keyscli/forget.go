// SPDX-License-Identifier: Apache-2.0

package keyscli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/savedwork"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/tracedest"
)

// ForgetAction is the audit action of keys forget, one event per org it changed.
const ForgetAction = "keys.forget"

// forgetActor names the command in the audit log (actor kind cli).
var forgetActor = audit.CLI("keys forget")

// lostRule is what keys forget does to a row sealed with the forgotten KEK: besides clearing the
// envelope, the mark it sets, and the key the audit summary lists the row under.
type lostRule struct {
	mark, summary string
}

// markLost is the mark of a credential that can be entered again.
const markLost = "credential_lost_at = now()"

// set is the SET clause of a row of c.
func (r lostRule) set(c Column) string { return c.Column + " = NULL, " + r.mark }

// lostRules covers every column of Columns. A credential becomes lost (credential_lost_at,
// migration 00052; the home repositories, 00054): the UI asks for it again and nothing serves it.
// Saved work cannot be entered again, so it expires as it does after its retention (status
// expired, the envelope dropped).
//
// Seam for ADR 0096 (MCP sign-in, draft #248), not built here: its sealed values (the access and
// refresh tokens, the pending PKCE verifier, admin-entered and dynamically registered client
// secrets) join [Columns] and get a rule here: a lost sign-in parks the runs that need it and
// posts "Sign in again"; a lost admin-entered client secret is entered again, a dynamically
// registered one registered again, and a pending flow started again (ADR 0096 section 4). TestColumnsListEveryBinding fails for a binding missing from Columns,
// and TestForgetHasARuleForEveryColumn for a column with no rule here.
var lostRules = map[Column]lostRule{
	{catalog.ProvidersTable, catalog.KeyColumn}:                {markLost, "providers"},
	{catalog.ProvidersTable, catalog.CostKeyColumn}:            {"cost_api_key_lost_at = now()", "providerCostKeys"},
	{connections.Table, connections.Column}:                    {markLost, "connections"},
	{savedwork.Table, savedwork.Column}:                        {"status = 'expired'", "savedWork"},
	{mcpservers.ServersTable, mcpservers.CredentialColumn}:     {markLost, "mcpServers"},
	{checkpoint.Table, checkpoint.Column}:                      {markLost, "homeRepositories"},
	{checkpoint.EpochsTable, checkpoint.Column}:                {markLost, "homeRepositories"},
	{tracedest.DestinationsTable, tracedest.CredentialsColumn}: {markLost, "traceDestinations"},
	{tracedest.PlatformTable, tracedest.CredentialsColumn}:     {markLost, "platformTraceDestination"},
}

// ForgetResult is what Forget changed.
type ForgetResult struct {
	// Lost counts the rows marked lost (or expired, for saved work), by audit summary key.
	Lost map[string]int
	// Orgs is how many orgs were changed (and audited).
	Orgs int
	// NewEpochs is how many agents got a new home repository epoch.
	NewEpochs int
	// Platform is whether the platform's trace destination was sealed with the KEK.
	Platform bool
}

// Forget gives up on a KEK that is lost (ADR 0085 decision 13): no copy of it exists any more, so
// the values it wraps can never be opened. Every value sealed with it is marked lost: provider,
// connection, MCP and trace credentials show "credential lost, enter it again", saved work
// expires, and each agent whose current home repository password was sealed with it gets a new
// repository epoch (a new kopia repository, sealed with the active KEK) and, when its sandbox is
// next created, a new home generation (ADR 0051): its earlier checkpoints cannot be restored. The
// old repositories are deleted after the org's home backup retention.
//
// The whole run holds the key events' advisory lock (Tool.begin), taken after the confirmation and
// before the first change: the KEK is checked against the key set loaded under it, so a keys add that
// restores it waits for Forget or makes Forget change nothing. Each org then changes in its own
// transaction, audited there as [ForgetAction] with the cli actor; the platform's trace destination,
// which belongs to no org, is one event in the install audit log (actor `cli keys forget`, ADR 0100
// decision 3), written last, in the locked transaction. A KEK still in the key set is not lost:
// Forget refuses it (rewrap, then retire it), also when it is restored while the confirmation is open.
// Without a terminal it needs --yes. Running Forget again finds nothing left to do.
func (t *Tool) Forget(ctx context.Context, id string) error {
	ks, err := t.Store.Load(ctx)
	if err != nil {
		return err
	}
	clearKeySet(ks) // only its IDs are read below
	usage, err := t.Usage(ctx)
	if err != nil {
		return err
	}
	if id == "" {
		if id, err = t.pickForgettable(ctx, ks, usage); err != nil {
			return err
		}
	}
	if !secretstore.ValidKEKID(id) {
		return fmt.Errorf("%q is not a valid KEK ID", id)
	}
	if _, ok := ks.Keys[id]; ok {
		return fmt.Errorf("KEK %s is in the key set, so it is not lost; to stop using it, run \"keys rewrap\" and then \"keys retire %s\"", id, id)
	}
	n := usage[id]
	if n == 0 {
		_, _ = fmt.Fprintf(t.Out, "No stored value is sealed with KEK %s; nothing to forget.\n", id)
		return nil
	}
	if err := checkLostRules(); err != nil {
		return err
	}
	if err := t.confirmForget(ctx, id, n); err != nil {
		return err
	}
	// The question can stay open for minutes, and the check above ran without the lock: the KEK may
	// have been restored from escrow meanwhile. From here to the end the key events' lock is held, so
	// the key set loaded under it is the one the whole run works with, and the new repositories are
	// sealed with the KEK active under it.
	c, ring, err := t.beginForget(ctx, id)
	if err != nil {
		return err
	}
	res, err := t.forget(ctx, c, ring, id)
	t.reportForget(id, res)
	return err
}

// beginForget takes the key events' lock for the whole of keys forget (Tool.begin) and checks the key
// set loaded under it: a KEK that is in it again is refused before anything changes. It returns the
// Change, which the caller ends, and the keyring of that key set.
func (t *Tool) beginForget(ctx context.Context, id string) (*Change, *secretstore.Keyring, error) {
	c, ks, _, err := t.begin(ctx, "forget")
	defer clearKeySet(ks)
	if err != nil {
		return nil, nil, err
	}
	if c == nil {
		return nil, nil, errors.New("keys forget needs the install audit chain")
	}
	if _, ok := ks.Keys[id]; ok {
		// Restored (keys add) after the check without the lock: the KEK is not lost. A keys.observed
		// this command found is still written.
		return nil, nil, errors.Join(errKEKRestored(id), c.Commit(ctx))
	}
	ring, err := t.ring(ks)
	if err != nil {
		return nil, nil, errors.Join(err, c.Commit(ctx))
	}
	return c, ring, nil
}

// errKEKRestored is the refusal when the KEK to forget is in the key set after all. It is returned
// under the key events' lock, before any org or platform part ran.
func errKEKRestored(id string) error {
	return fmt.Errorf("KEK %s is in the key set again, so it is not lost; nothing was changed", id)
}

// errForgetUnconfirmed refuses keys forget where nobody can be asked and --yes was not given.
var errForgetUnconfirmed = errors.New("keys forget cannot be undone: run it on a terminal (kubectl exec -it) or pass --yes")

// confirmForget asks the person to type the KEK ID: forgetting cannot be undone. Unlike the other
// commands it does not run unasked without a terminal: only --yes skips the question.
func (t *Tool) confirmForget(ctx context.Context, id string, n int) error {
	if t.Yes {
		return nil
	}
	if t.Prompt == nil {
		return errForgetUnconfirmed
	}
	got, err := t.Prompt.Input(ctx,
		fmt.Sprintf("Forget KEK %s and mark the %d value(s) it seals as lost? Type the KEK ID to continue", id, n),
		"This cannot be undone. People enter the lost credentials again; saved work sealed with it expires; the agents' earlier home checkpoints cannot be restored. Only do this when no copy of the KEK exists, not even in your backup escrow.",
		func(s string) error {
			if strings.TrimSpace(s) != id {
				return fmt.Errorf("type %s to continue", id)
			}
			return nil
		})
	if err != nil {
		return err
	}
	if strings.TrimSpace(got) != id {
		return cli.ErrCancelled
	}
	return nil
}

// checkLostRules refuses a run when a column of Columns has no rule in lostRules.
func checkLostRules() error {
	for _, c := range Columns {
		if _, ok := lostRules[c]; !ok {
			return fmt.Errorf("keys forget has no rule for %s.%s", c.Table, c.Column)
		}
	}
	return nil
}

// forget runs the org parts and then the platform part, all while c holds the key events' lock, and
// ends c. The lock order is the one of ADR 0100 decision 1.7 across the whole run: c's transaction
// holds only the advisory lock while the orgs change (it has written nothing yet, so it holds no
// audit head), each org's transaction takes its rows and its org head and commits, and only then does
// c take the platform row and the install head. A writer that waits for the key events' lock takes it
// first in its transaction, so it holds nothing an org part needs.
func (t *Tool) forget(ctx context.Context, c *Change, ring *secretstore.Keyring, id string) (ForgetResult, error) {
	res := ForgetResult{Lost: map[string]int{}}
	orgs, err := t.orgsSealedWith(ctx, id)
	if err != nil {
		return res, errors.Join(err, c.Commit(ctx))
	}
	settings := orgsettings.NewService(t.Pool)
	var errs []error
	for _, org := range orgs {
		if err := t.forgetOrg(ctx, ring, settings, org, id, &res); err != nil {
			errs = append(errs, fmt.Errorf("org %s: %w", org.String(), err))
		}
	}
	if err := t.forgetPlatform(ctx, c, id, &res); err != nil {
		errs = append(errs, fmt.Errorf("platform trace destination: %w", err))
	}
	return res, errors.Join(errs...)
}

// forgetPlatform marks the platform's trace destination lost when id sealed it, and ends c. It belongs
// to no org, so it is one event in the install audit chain (ADR 0100 decision 3), and it follows the
// pattern of the other key commands (Tool.begin, Auditor.Baseline, Change.Record, Change.Commit): c
// has held the key events' advisory lock since before the org parts; it writes keys.observed with the
// command's actor when the live KEKs differed from the last recorded state (a change whose event was
// lost, as when the KEK was removed from the Secret by hand), and then updates the row and writes its
// own event. It needs no platform admin, because the install chain has none to name (decision 1.4).
// Each org's part is audited in that org's own chain by forgetOrg.
func (t *Tool) forgetPlatform(ctx context.Context, c *Change, id string, res *ForgetResult) error {
	platform := Column{tracedest.PlatformTable, tracedest.CredentialsColumn}
	changed, err := t.markPlatformLost(ctx, c, platform, id)
	if err != nil {
		// Only the update and its event are undone, in their savepoint; the keys.observed this command
		// found is still written by Commit.
		return errors.Join(err, c.Commit(ctx))
	}
	if err := c.Commit(ctx); err != nil {
		return err
	}
	if changed {
		res.Platform = true
		res.Lost["platformTraceDestination"]++
	}
	return nil
}

// markPlatformLost updates the platform row and records keys.forget in one savepoint of c's
// transaction: the row change and its event stand or fall together. The row is locked first, in c's
// transaction, and the pending keys.observed written after it and before the savepoint opens: the
// order of decision 1.7 (a row lock, then the install head, as SetPlatformDestination takes them), and
// a failure in the savepoint leaves the keys.observed alone.
func (t *Tool) markPlatformLost(ctx context.Context, c *Change, platform Column, id string) (changed bool, err error) {
	rows, err := c.tx.Query(ctx, fmt.Sprintf(`SELECT 1 FROM %s WHERE %s->>'kek_id' = $1 FOR UPDATE`, platform.Table, platform.Column), id)
	if err != nil {
		return false, err
	}
	locked, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		return false, err
	}
	if len(locked) == 0 {
		return false, nil // not sealed with id; Commit writes a pending keys.observed
	}
	// The pending keys.observed goes in before the savepoint, so undoing it cannot take it along.
	if err := c.flush(ctx); err != nil {
		return false, err
	}
	sp, err := c.tx.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			_ = sp.Rollback(ctx)
		}
	}()
	tag, err := sp.Exec(ctx, fmt.Sprintf(`UPDATE %s SET %s WHERE %s->>'kek_id' = $1`,
		platform.Table, lostRules[platform].set(platform), platform.Column), id)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, sp.Commit(ctx)
	}
	if err = c.Record(ctx, Event{
		Command: "forget", Action: ForgetAction, TargetKind: "kek", TargetID: id,
		After: map[string]any{"lost": map[string][]string{"platformTraceDestination": {"platform"}}},
	}); err != nil {
		return false, err
	}
	return true, sp.Commit(ctx)
}

// orgsSealedWith lists the orgs with a value sealed with id.
func (t *Tool) orgsSealedWith(ctx context.Context, id string) ([]pgtype.UUID, error) {
	var parts []string
	for _, c := range Columns {
		if !c.singleton() {
			parts = append(parts, fmt.Sprintf(`SELECT org_id FROM %s WHERE %s->>'kek_id' = $1`, c.Table, c.Column))
		}
	}
	rows, err := t.Pool.Query(ctx, strings.Join(parts, " UNION ")+" ORDER BY 1", id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[pgtype.UUID])
}

// forgetOrg marks the org's values sealed with id lost, starts a new repository epoch for each
// agent whose current repository's password was among them, and audits both, in one transaction.
func (t *Tool) forgetOrg(ctx context.Context, ring *secretstore.Keyring, settings *orgsettings.Service, org pgtype.UUID, id string, res *ForgetResult) error {
	retain, err := settings.HomeBackupRetention(ctx, org)
	if err != nil {
		return err
	}
	lost := map[string][]string{}
	var epochs []string
	_, err = audit.InPgTx(ctx, t.Pool, func(tx pgx.Tx, b audit.Batch) error {
		lost, epochs = map[string][]string{}, nil
		agents := map[string]pgtype.UUID{}
		for _, c := range Columns {
			if c.singleton() {
				continue
			}
			rule := lostRules[c]
			agentCol := c.rowKey()
			if c.Table == checkpoint.Table || c.Table == checkpoint.EpochsTable {
				agentCol = "agent_id"
			}
			rows, err := tx.Query(ctx, fmt.Sprintf(`UPDATE %[1]s SET %[3]s WHERE org_id = $1 AND %[2]s->>'kek_id' = $2 RETURNING %[4]s::text, %[5]s::text`,
				c.Table, c.Column, rule.set(c), c.rowKey(), agentCol), org, id)
			if err != nil {
				return fmt.Errorf("%s: %w", c.Table, err)
			}
			for rows.Next() {
				var row, agent string
				if err := rows.Scan(&row, &agent); err != nil {
					rows.Close()
					return err
				}
				if c.Table == checkpoint.Table {
					row = checkpoint.EpochRow(agent, 0)
				}
				lost[rule.summary] = append(lost[rule.summary], row)
				if agentCol == "agent_id" {
					var a pgtype.UUID
					if err := a.Scan(agent); err != nil {
						rows.Close()
						return err
					}
					agents[agent] = a
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return fmt.Errorf("%s: %w", c.Table, err)
			}
		}
		q := store.New(tx)
		pw := checkpoint.Passwords{Q: q, Keys: ring}
		for _, key := range slices.Sorted(maps.Keys(agents)) {
			agent := agents[key]
			lostNow, err := currentRepositoryLost(ctx, q, org, agent)
			if err != nil {
				return err
			}
			if !lostNow {
				continue // a retired epoch's password was lost; the current one still opens
			}
			r, ok, err := pw.NewEpochIn(ctx, q, org, agent, checkpoint.ReasonKeyLost, retain)
			if err != nil {
				return fmt.Errorf("new repository epoch for agent %s: %w", key, err)
			}
			if ok {
				epochs = append(epochs, checkpoint.EpochRow(key, r.Epoch))
			}
		}
		if len(lost) == 0 {
			return nil // changed meanwhile: nothing of this org is sealed with id any more
		}
		after := map[string]any{"lost": lost, "retentionDays": int(retain.Hours() / 24)}
		if len(epochs) > 0 {
			after["newRepositoryEpochs"] = epochs
		}
		b.Org(audit.Event{
			Org: org, Actor: forgetActor, Action: ForgetAction, TargetKind: "kek", TargetID: id, After: after,
		})
		return nil
	})
	if err != nil {
		return err
	}
	if len(lost) > 0 {
		res.Orgs++
	}
	for k, rows := range lost {
		res.Lost[k] += len(rows)
	}
	res.NewEpochs += len(epochs)
	return nil
}

// currentRepositoryLost reports whether the agent's current repository (its highest epoch, else
// epoch 0) has lost its password.
func currentRepositoryLost(ctx context.Context, q *store.Queries, org, agent pgtype.UUID) (bool, error) {
	e, err := q.LatestHomeRepositoryEpoch(ctx, store.LatestHomeRepositoryEpochParams{OrgID: org, AgentID: agent})
	if err == nil {
		return e.CredentialLostAt.Valid, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	h, err := q.GetHomeRepository(ctx, store.GetHomeRepositoryParams{OrgID: org, AgentID: agent})
	if err != nil {
		return false, err
	}
	return h.CredentialLostAt.Valid, nil
}

// reportForget prints what Forget changed and what the people of each org do next.
func (t *Tool) reportForget(id string, res ForgetResult) {
	total := 0
	for _, n := range res.Lost {
		total += n
	}
	_, _ = fmt.Fprintf(t.Out, "Forgot KEK %s: %d value(s) marked lost in %d org(s).\n", id, total, res.Orgs)
	for _, k := range slices.Sorted(maps.Keys(res.Lost)) {
		_, _ = fmt.Fprintf(t.Out, "  %-26s %d\n", k, res.Lost[k])
	}
	if res.NewEpochs > 0 {
		_, _ = fmt.Fprintf(t.Out, "%d agent(s) start a new home repository; their earlier checkpoints cannot be restored.\n", res.NewEpochs)
	}
	if total > 0 {
		_, _ = fmt.Fprintln(t.Out, "Org admins see \"credential lost\" on each provider, connection, MCP server and trace destination concerned; enter those credentials again.")
	}
	if res.Platform {
		_, _ = fmt.Fprintln(t.Out, "The platform's trace destination lost its credentials: a platform admin enters them again. (Recorded in the install audit log.)")
	}
}

// pickForgettable lists the KEK IDs that seal stored values but are not in the key set.
func (t *Tool) pickForgettable(ctx context.Context, ks KeySet, usage map[string]int) (string, error) {
	if t.Prompt == nil {
		return "", errors.New("a KEK ID is required")
	}
	var choices []cli.Choice
	for _, id := range slices.Sorted(maps.Keys(usage)) {
		if _, ok := ks.Keys[id]; !ok && usage[id] > 0 {
			choices = append(choices, cli.Choice{Label: fmt.Sprintf("%s  seals %d value(s), not in the key set", id, usage[id]), Value: id})
		}
	}
	if len(choices) == 0 {
		return "", errors.New("every KEK that seals a stored value is in the key set; nothing is lost")
	}
	return t.Prompt.Select(ctx, "Which lost KEK should be forgotten?", choices)
}
