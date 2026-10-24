// SPDX-License-Identifier: Apache-2.0

package agentd

// Saving a stuck sandbox's uncommitted work (plan item AG1; ADR 0054).
//
// Before the sandbox is recreated, agentd reads the workspace's uncommitted changes with
// savework.sh, within SaveTimeout (30 s), and seals them in Postgres (package savedwork). Each
// repository becomes one saved_work row and a system-made approval request, "Push saved work to
// a branch", on the ticket: an approver decides, and only then does the API push
// koinon/wip/<ticket>-<time> to GitHub (package approvals). Nothing leaves the sandbox for
// GitHub without that approval.
//
// Saving is best effort. If it fails, runs out of time, finds more than 50 MB, or the agent has no
// GitHub access to the repository, the incident says the work was lost and why, and the ticket's
// timeline says so.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/internal/savedwork"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// savedRow is one repository's sealed work, ready to insert.
type savedRow struct {
	id     pgtype.UUID
	work   savedwork.Work
	env    secretstore.Envelope
	branch string
}

// saveResult is what became of the workspace's uncommitted work.
type saveResult struct {
	status string // saved, nothing or lost
	notes  []string
	rows   []savedRow
}

func (s saveResult) note() string { return strings.Join(s.notes, "; ") }

var errSaveOver = errors.New("the uncommitted work is larger than 50 MB")

// saveWork reads and seals the workspace's uncommitted work. It never fails the caller: the
// outcome says what happened.
func (r *runner) saveWork(ctx context.Context, workspace, sandbox string) saveResult {
	lost := func(format string, a ...any) saveResult {
		return saveResult{status: "lost", notes: []string{fmt.Sprintf(format, a...)}}
	}
	if r.p.cfg.Keyring == nil {
		return lost("saving work needs the key-encryption keys, which this koinon-agentd does not have")
	}
	sctx, cancel := context.WithTimeout(ctx, r.p.cfg.SaveTimeout)
	defer cancel()
	cwd := r.cwd
	if cwd == "" {
		cwd = r.p.cfg.SessionCwd
	}
	out, code, stderr, err := r.sandboxExecLimit(sctx, workspace, sandbox,
		[]string{"/bin/sh", "-c", saveworkScript, "savework", cwd, strconv.Itoa(savedwork.MaxBytes)},
		nil, r.p.cfg.SaveTimeout, savedwork.MaxBytes+(8<<20))
	switch {
	case sctx.Err() != nil:
		return lost("reading the workspace took longer than %s (its storage may be what the stuck process waits on)", r.p.cfg.SaveTimeout)
	case err != nil:
		return lost("reading the workspace failed: %v", err)
	case code == 3:
		return lost("%s", errSaveOver)
	case code != 0:
		return lost("reading the workspace failed (exit %d): %s", code, strings.TrimSpace(stderr))
	}
	works, skipped, err := parseSaveOutput(out)
	if err != nil {
		return lost("reading the workspace failed: %v", err)
	}
	bound := r.githubTargets()
	res := saveResult{notes: skipped}
	now := time.Now()
	for _, w := range works {
		if !bound[strings.ToLower(w.Manifest.Repo)] {
			res.notes = append(res.notes, fmt.Sprintf("%s: this agent has no GitHub access to the repository, so its work is not saved", w.Manifest.Repo))
			continue
		}
		id := pgtype.UUID{Bytes: uuid.Must(uuid.NewV7()), Valid: true}
		env, err := savedwork.Seal(r.p.cfg.Keyring, r.run.OrgID.String(), id.String(), w)
		if err != nil {
			res.notes = append(res.notes, fmt.Sprintf("%s: %v", w.Manifest.Repo, err))
			continue
		}
		branch := savedwork.BranchName(r.run.TicketID.String(), now)
		if len(res.rows) > 0 {
			branch += "-" + strconv.Itoa(len(res.rows)+1)
		}
		res.rows = append(res.rows, savedRow{id: id, work: w, env: env, branch: branch})
	}
	switch {
	case len(res.rows) > 0:
		res.status = "saved"
	case len(works) == 0 && len(skipped) == 0:
		res.status = "nothing"
	default:
		res.status = "lost"
	}
	return res
}

// githubTargets are the repositories (lower case owner/name) the run's agent version is bound to.
func (r *runner) githubTargets() map[string]bool {
	out := map[string]bool{}
	if b, ok := r.vcfg.Binding(connectors.GitHubID); ok {
		for _, t := range b.Targets {
			out[strings.ToLower(t)] = true
		}
	}
	return out
}

// insertSavedWork stores a sealed work and asks for the approval to push it.
func (r *runner) insertSavedWork(ctx context.Context, q *store.Queries, w savedRow) error {
	env, err := json.Marshal(w.env)
	if err != nil {
		return err
	}
	manifest, err := json.Marshal(w.work.Manifest)
	if err != nil {
		return err
	}
	expires := time.Now().Add(savedwork.Retention)
	if _, err := q.InsertSavedWork(ctx, store.InsertSavedWorkParams{
		OrgID: r.run.OrgID, ID: w.id, TicketID: r.run.TicketID, RunID: r.run.ID, AgentID: r.run.AgentID,
		Repo: w.work.Manifest.Repo, BaseSha: w.work.Manifest.Base, Branch: w.branch, Manifest: manifest,
		SizeBytes: w.work.Manifest.Bytes(), Envelope: env, ExpiresAt: pgtype.Timestamptz{Time: expires, Valid: true},
	}); err != nil {
		return err
	}
	args := savedwork.PushArgsJSON(w.id.String(), w.work.Manifest.Repo, w.work.Manifest.Base, w.branch)
	sum := sha256.Sum256([]byte(args))
	row, err := q.CreateSystemApprovalRequest(ctx, store.CreateSystemApprovalRequestParams{
		OrgID: r.run.OrgID, TicketID: r.run.TicketID, RunID: r.run.ID, AgentID: r.run.AgentID,
		Connector: connectors.GitHubID, Tool: savedwork.ToolPush, Target: connectors.GitHubID + ":" + w.work.Manifest.Repo,
		ArgsJson: args, ArgsSha256: hex.EncodeToString(sum[:]), ExpiresAt: pgtype.Timestamptz{Time: expires, Valid: true},
	})
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"approval_id": row.ID.String(), "tool": row.Tool, "target": row.Target, "args_sha256": row.ArgsSha256,
		"decision": string(row.Decision), "risk": json.RawMessage(`{}`), "system": true,
	})
	if err != nil {
		return err
	}
	_, err = q.AppendTicketEvent(ctx, store.AppendTicketEventParams{
		OrgID: r.run.OrgID, TicketID: r.run.TicketID, RunID: r.run.ID,
		Kind: githubtools.KindApprovalRequested, ActorKind: store.TicketActorKindSystem, Payload: payload,
	})
	return err
}

// parseSaveOutput reads savework.sh's output.
func parseSaveOutput(out []byte) ([]savedwork.Work, []string, error) {
	var works []savedwork.Work
	var skipped []string
	var cur *savedwork.Work
	for len(out) > 0 {
		line, rest, _ := bytes.Cut(out, []byte("\n"))
		out = rest
		f := strings.Fields(string(line))
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "R":
			if len(f) != 3 || cur != nil {
				return nil, nil, fmt.Errorf("unexpected line %q", line)
			}
			cur = &savedwork.Work{Manifest: savedwork.Manifest{Repo: f[1], Base: f[2]}, Content: map[string][]byte{}}
		case "E":
			if cur == nil {
				return nil, nil, fmt.Errorf("unexpected end of a repository")
			}
			works = append(works, *cur)
			cur = nil
		case "S":
			skipped = append(skipped, strings.TrimSpace(strings.TrimPrefix(string(line), "S ")))
		case "F":
			if cur == nil {
				return nil, nil, fmt.Errorf("a file outside a repository")
			}
			// F STATUS MODE SIZE ADDED DELETED BINARY PATH...
			parts := strings.SplitN(string(line), " ", 8)
			if len(parts) != 8 {
				return nil, nil, fmt.Errorf("unexpected line %q", line)
			}
			size, err1 := strconv.ParseInt(parts[3], 10, 64)
			added, err2 := strconv.Atoi(parts[4])
			deleted, err3 := strconv.Atoi(parts[5])
			if err := errors.Join(err1, err2, err3); err != nil || size < 0 || size > savedwork.MaxBytes {
				return nil, nil, fmt.Errorf("unexpected line %q", line)
			}
			file := savedwork.File{
				Path: parts[7], Status: parts[1], Mode: parts[2], Size: size, Added: added, Deleted: deleted, Binary: parts[6] == "1",
			}
			if file.Status == savedwork.Deleted {
				file.Mode, file.Size = "", 0
			} else {
				if int64(len(out)) < size+1 {
					return nil, nil, errors.New("the output ends inside a file")
				}
				cur.Content[file.Path] = append([]byte(nil), out[:size]...)
				out = out[size+1:]
			}
			cur.Manifest.Files = append(cur.Manifest.Files, file)
		default:
			return nil, nil, fmt.Errorf("unexpected line %q", line)
		}
	}
	if cur != nil {
		return nil, nil, errors.New("the output ends inside a repository")
	}
	return works, skipped, nil
}
