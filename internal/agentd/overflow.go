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

// inputOverflow is what prompt found: the inputs over the cap.
type inputOverflow struct {
	texts  []string
	over   []contextguard.Over
	window int64
}

// guardInputs records on the runner the inputs of the prompt that are over the cap.
func (r *runner) guardInputs(texts []string) {
	if over := contextguard.Check(texts, r.window); len(over) > 0 {
		r.overflow = &inputOverflow{texts: texts, over: over, window: r.window}
	}
}

// artifactDir is the directory of the session that holds the inputs the guard kept.
func artifactDir(cwd string) string { return strings.TrimSuffix(cwd, "/") + "/inputs" }

// parkOnOverflow keeps each oversized input, asks a person and parks the run. through is the
// message seq the prompt covers: the run does not read these messages again when it resumes.
func (r *runner) parkOnOverflow(ctx context.Context, task store.Task, vc agentconfig.Config, through int64, closeHarness func()) error {
	ov := r.overflow
	var lines []string
	var ids []string
	for _, o := range ov.over {
		body := []byte(ov.texts[o.Index])
		if len(body) > maxArtifactBytes {
			return &failure{reason: fmt.Sprintf("an input of %d bytes is over the %d bytes Koinon keeps", len(body), maxArtifactBytes),
				class: store.RunFailureClassContextOverflow}
		}
		sum := sha256.Sum256(body)
		art, err := r.q.CreateRunArtifact(ctx, store.CreateRunArtifactParams{
			OrgID: r.run.OrgID, RunID: r.run.ID, TaskID: task.ID, Kind: "input", Name: fmt.Sprintf("input %d", o.Index+1),
			MediaType: "text/plain; charset=utf-8", SizeBytes: int64(len(body)), Sha256: hex.EncodeToString(sum[:]), Body: body,
		})
		if err != nil {
			return err
		}
		path := fmt.Sprintf("%s/%s.txt", artifactDir(r.cwd), art.ID.String())
		if err := r.writeArtifact(ctx, path, body); err != nil {
			// The artifact is kept in the database either way; the person is told where the file is not.
			r.log.Warn("write the oversized input to the session directory", "artifact", art.ID.String(), "err", err)
			path = ""
		}
		ids = append(ids, art.ID.String())
		line := fmt.Sprintf("- %d bytes (about %d tokens) from the task's messages, kept whole as run artifact %s", len(body), o.Tokens, art.ID.String())
		if path != "" {
			line += ", readable in parts at " + path
		}
		lines = append(lines, line)
	}
	text := fmt.Sprintf("[Koinon] A new input is larger than 80%% of the model's context window (%d tokens), so Koinon did not "+
		"send it to the model. Nothing was cut: the whole input is kept.\n%s\n"+
		"Tell the agent what to do with it: for example to read it in parts and summarize it, to use only a part, or to skip it.",
		contextguardWindow(ov.window), strings.Join(lines, "\n"))
	target := questions.TaskTarget(task.NotifyKind, task.NotifyUserID, task.NotifyRole)
	recipients, err := questions.Recipients(ctx, r.q, r.run.OrgID, target)
	if err != nil {
		return err
	}
	if len(recipients) == 0 {
		target = questions.DefaultTarget
		if recipients, err = questions.Recipients(ctx, r.q, r.run.OrgID, target); err != nil {
			return err
		}
	}
	if len(recipients) == 0 {
		return &failure{reason: "an input is larger than the model's context cap and nobody in the org can be asked what to do with it",
			class: store.RunFailureClassContextOverflow}
	}
	reminder, ttl := vc.QuestionTimers()
	now := time.Now()
	remindAt := time.Time{}
	if reminder > 0 {
		remindAt = now.Add(reminder)
	}
	if _, _, err := questions.Create(ctx, r.q, questions.Ask{
		Org: r.run.OrgID, Task: task.ID, Run: r.run.ID, Agent: r.run.AgentID, Target: target, Recipients: recipients,
		Question:  text,
		Default:   "Skip the oversized input: do not read it, and tell the user it was not processed.",
		ExpiresAt: now.Add(ttl), RemindAt: remindAt,
	}); err != nil {
		return err
	}
	if _, err := r.q.SetRunPromptThrough(ctx, store.SetRunPromptThroughParams{
		OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch, PromptThroughSeq: through,
	}); err != nil {
		return err
	}
	if err := r.own.Record(ctx, kindInputOverflow, map[string]any{
		"artifacts": ids, "window": contextguardWindow(ov.window), "capTokens": contextguard.Cap(ov.window), "throughSeq": through,
	}); err != nil {
		return err
	}
	closeHarness()
	// The turn's end opens the question and parks the run (reason input_required).
	return r.endTurn(ctx)
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
