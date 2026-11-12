// SPDX-License-Identifier: Apache-2.0

package agentd

// The context guard (#22): an input larger than 80% of the model's context window is not sent.
//
// An input is one message on the task (a webhook body, a chat paste). When one is over the cap
// (internal/contextguard), agentd keeps its full body as a run artifact, writes it to the session
// directory so the agent can read it in parts, asks a person what to do (an ask_user question, so
// the run parks with reason input_required and the task moves to input-required) and does not send
// the prompt. Nothing is truncated: the artifact is the whole body. Design adapted from
// hydro-project/infinity (Apache-2.0).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/contextguard"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/questions"
)

// kindInputOverflow is the run event of an input the guard held back.
const kindInputOverflow = "run.input_overflow"

// artifactChunk is how much of an artifact one exec writes: OpenShell's exec stdin is capped
// (about 4 MiB) and the command must consume each chunk (AGENTS.md, working rules).
const artifactChunk = 1 << 20

// maxArtifactBytes bounds one input artifact (the 16 MiB a task message cannot exceed anyway,
// with room); a larger input fails the run instead of filling the database.
const maxArtifactBytes = 64 << 20

// inputOverflow is what prompt found: the inputs over the cap, by their message seqs.
type inputOverflow struct {
	texts  []string
	seqs   []int64 // the timeline seq of each text
	over   []contextguard.Over
	window int64
	after  int64 // the seq the prompt read from: the run's cursor after the park
}

// guardInputs records on the runner the inputs of the prompt that are over the cap.
func (r *runner) guardInputs(texts []string, seqs []int64, after int64) {
	if over := contextguard.Check(texts, r.window); len(over) > 0 {
		r.overflow = &inputOverflow{texts: texts, seqs: seqs, over: over, window: r.window, after: after}
	}
}

// artifactDir is the directory of the session that holds the inputs the guard kept.
func artifactDir(cwd string) string { return strings.TrimSuffix(cwd, "/") + "/inputs" }

func (r *runner) artifactPath(id string) string { return artifactDir(r.cwd) + "/" + id + ".txt" }

type keptInput struct {
	id   string
	body []byte
}

// parkOnOverflow keeps each oversized input, asks a person and parks the run. The artifacts, the
// question and the run's cursor are written in one transaction: a retry, a takeover or a drain finds
// them whole or finds none. The cursor stays where the prompt started, so the messages under the cap
// that were read with the oversized one are sent when the run resumes; the oversized message itself is
// skipped then, because it has an artifact. sweep frees the agent's run slot before the park, as the
// normal end of a turn does.
func (r *runner) parkOnOverflow(ctx context.Context, task store.Task, vc agentconfig.Config, closeHarness, sweep func()) error {
	ov := r.overflow
	for _, o := range ov.over {
		if n := len(ov.texts[o.Index]); n > maxArtifactBytes {
			return &failure{
				reason: fmt.Sprintf("an input of %d bytes is over the %d bytes Koinon keeps", n, maxArtifactBytes),
				class:  store.RunFailureClassContextOverflow,
			}
		}
	}
	var kept []keptInput
	err := pgx.BeginFunc(ctx, r.p.db, func(tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		var lines []string
		for _, o := range ov.over {
			body := []byte(ov.texts[o.Index])
			sum := sha256.Sum256(body)
			art, err := q.CreateRunArtifact(ctx, store.CreateRunArtifactParams{
				OrgID: r.run.OrgID, RunID: r.run.ID, TaskID: task.ID, Kind: "input", MessageSeq: ov.seqs[o.Index],
				Name: fmt.Sprintf("message %d", ov.seqs[o.Index]), MediaType: "text/plain; charset=utf-8",
				SizeBytes: int64(len(body)), Sha256: hex.EncodeToString(sum[:]), Body: body,
			})
			if err != nil {
				return err
			}
			id := art.ID.String()
			kept = append(kept, keptInput{id: id, body: body})
			lines = append(lines, fmt.Sprintf("- %d bytes (about %d tokens), kept whole as run artifact %s, readable in parts at %s",
				len(body), o.Tokens, id, r.artifactPath(id)))
		}
		text := fmt.Sprintf("[Koinon] A new input is larger than 80%% of the model's context window (%d tokens), so Koinon did not "+
			"send it to the model. Nothing was cut: the whole input is kept.\n%s\n"+
			"Tell the agent what to do with it: for example to read it in parts and summarize it, to use only a part, or to skip it.",
			contextguardWindow(ov.window), strings.Join(lines, "\n"))
		target := questions.TaskTarget(task.NotifyKind, task.NotifyUserID, task.NotifyRole)
		recipients, err := questions.Recipients(ctx, q, r.run.OrgID, target)
		if err != nil {
			return err
		}
		if len(recipients) == 0 {
			target = questions.DefaultTarget
			if recipients, err = questions.Recipients(ctx, q, r.run.OrgID, target); err != nil {
				return err
			}
		}
		if len(recipients) == 0 {
			return &failure{
				reason: "an input is larger than the model's context cap and nobody in the org can be asked what to do with it",
				class:  store.RunFailureClassContextOverflow,
			}
		}
		reminder, ttl := vc.QuestionTimers()
		now := time.Now()
		remindAt := time.Time{}
		if reminder > 0 {
			remindAt = now.Add(reminder)
		}
		if _, _, err := questions.Create(ctx, q, questions.Ask{
			Org: r.run.OrgID, Task: task.ID, Run: r.run.ID, Agent: r.run.AgentID, Target: target, Recipients: recipients,
			Question:  text,
			Default:   "Skip the oversized input: do not read it, and tell the user it was not processed.",
			ExpiresAt: now.Add(ttl), RemindAt: remindAt,
		}); err != nil {
			return err
		}
		// The cursor stays where this prompt started: the run resumes through the resume path and reads
		// the messages under the cap again.
		_, err = q.SetRunPromptThrough(ctx, store.SetRunPromptThroughParams{
			OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch, PromptThroughSeq: ov.after,
		})
		return err
	})
	if err != nil {
		return err
	}
	// The files are written after the commit and again on resume when missing (rehydrate), so a failure
	// here loses nothing: the artifact is in the database.
	for _, k := range kept {
		if err := r.writeArtifact(ctx, r.artifactPath(k.id), k.body); err != nil {
			r.log.Warn("write the oversized input to the session directory", "artifact", k.id, "err", err)
		}
	}
	ids := make([]string, len(kept))
	for i, k := range kept {
		ids[i] = k.id
	}
	if err := r.own.Record(ctx, kindInputOverflow, map[string]any{
		"artifacts": ids, "window": contextguardWindow(ov.window), "windowFromCatalog": ov.window > 0,
		"capTokens": contextguard.Cap(ov.window), "resumeFromSeq": ov.after,
	}); err != nil {
		return err
	}
	closeHarness()
	sweep()
	// The prompt was not sent, so the outcomes it carried are not delivered: they ride the resume prompt.
	r.delivered, r.deliveredQ = nil, nil
	// The turn's end opens the question and parks the run (reason input_required).
	return r.endTurn(ctx)
}

// rehydrate writes the task's kept inputs that are not in the session directory (a sandbox that was
// recreated, a park that outlived its file), so the agent can read them in parts. Best effort.
func (r *runner) rehydrate(ctx context.Context, arts []store.ListTaskInputArtifactsRow) {
	for _, a := range arts {
		path := r.artifactPath(a.ID.String())
		if _, code, _, err := r.sandboxExecLimit(ctx, r.sbWorkspace, r.sbName, []string{"/bin/sh", "-c", `test -f "$1"`, "sh", path}, nil, time.Minute, 1<<10); err == nil && code == 0 {
			continue
		}
		full, err := r.q.GetRunArtifact(ctx, store.GetRunArtifactParams{OrgID: r.run.OrgID, ID: a.ID})
		if err == nil {
			err = r.writeArtifact(ctx, path, full.Body)
		}
		if err != nil {
			r.log.Warn("restore a kept input to the session directory", "artifact", a.ID.String(), "err", err)
		}
	}
}

// contextguardWindow is the window the cap was computed from (the default when the catalog has none).
func contextguardWindow(w int64) int64 {
	if w <= 0 {
		return contextguard.DefaultWindow
	}
	return w
}

// writeArtifact writes body to path in the sandbox, one chunk per exec; the first chunk creates the file.
func (r *runner) writeArtifact(ctx context.Context, path string, body []byte) error {
	for off := 0; off == 0 || off < len(body); off += artifactChunk {
		end := min(off+artifactChunk, len(body))
		redirect := ">>"
		if off == 0 {
			redirect = ">"
		}
		_, code, stderr, err := r.sandboxExecLimit(ctx, r.sbWorkspace, r.sbName,
			[]string{"/bin/sh", "-c", `umask 077 && mkdir -p "$(dirname "$1")" && cat ` + redirect + ` "$1"`, "sh", path},
			body[off:end], 5*time.Minute, 1<<20)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("exit %d: %s", code, strings.TrimSpace(stderr))
		}
	}
	return nil
}
