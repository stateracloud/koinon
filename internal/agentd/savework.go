// SPDX-License-Identifier: Apache-2.0

package agentd

// Saving a stuck sandbox's uncommitted work (plan item AG1; ADR 0054).
//
// Before the sandbox is recreated, agentd reads the workspace's uncommitted changes with
// savework.sh, within SaveTimeout (30 s), and posts each repository's bundle to koinon-api
// (package savedwork), which seals it - koinon-agentd holds no key-encryption key - and stores it
// as one saved_work row and a system-made approval request, "Push saved work to a branch", on the
// task: an approver decides, and only then does the API push
// koinon/wip/<task>-<time> to GitHub (package approvals). Nothing leaves the sandbox for
// GitHub without that approval.
//
// Saving is best effort. If it fails, runs out of time, finds more than 50 MB, or the agent has no
// GitHub access to the repository, the incident says the work was lost and why, and the task's
// timeline says so.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/savedwork"
)

// savedRow is one repository's work, stored by koinon-api.
type savedRow struct {
	id         pgtype.UUID
	approvalID string
	branch     string
}

// saveResult is what became of the workspace's uncommitted work.
type saveResult struct {
	status string // saved, nothing or lost
	notes  []string
	rows   []savedRow
}

// note is the sandbox-reported detail for the timeline and the incident, capped: the lines come
// from the sandbox, so they are shown as reported by it and bounded in number and length.
func (s saveResult) note() string { return strings.Join(capNotes(s.notes), "; ") }

// capNotes keeps at most savedwork.MaxNotes notes of savedwork.MaxNoteBytes bytes each.
func capNotes(notes []string) []string {
	var out []string
	for i, n := range notes {
		if i == savedwork.MaxNotes {
			out = append(out, fmt.Sprintf("and %d more", len(notes)-i))
			break
		}
		out = append(out, strings.ToValidUTF8(cutBytes(strings.Join(strings.Fields(n), " "), savedwork.MaxNoteBytes), ""))
	}
	return out
}

var errSaveOver = errors.New("the uncommitted work is larger than 50 MB")

// saveWork reads and seals the workspace's uncommitted work. It never fails the caller: the
// outcome says what happened.
func (r *runner) saveWork(ctx context.Context, workspace, sandbox string) saveResult {
	lost := func(format string, a ...any) saveResult {
		return saveResult{status: "lost", notes: []string{fmt.Sprintf(format, a...)}}
	}
	if r.p.cfg.SavedWorkURL == "" || r.p.cfg.SavedWorkKey == nil {
		return lost("saving work is not configured on this koinon-agentd (no koinon-api address)")
	}
	sctx, cancel := context.WithTimeout(ctx, r.p.cfg.SaveTimeout)
	defer cancel()
	cwd := r.cwd
	if cwd == "" {
		cwd = r.p.cfg.SessionCwd
	}
	host, err := r.codeHost(sctx)
	if err != nil {
		return lost("reading the code host of the agent's connection failed: %v", err)
	}
	started := time.Now()
	out, code, stderr, err := r.sandboxExecLimit(sctx, workspace, sandbox,
		[]string{"/bin/sh", "-c", saveworkScript, "savework", cwd, strconv.Itoa(savedwork.MaxBytes), host.origin()},
		nil, r.p.cfg.SaveTimeout, savedwork.MaxBytes+(8<<20))
	switch {
	case sctx.Err() != nil || (err != nil && time.Since(started) >= r.p.cfg.SaveTimeout):
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
	var key []byte
	for _, w := range works {
		if !bound[strings.ToLower(w.Manifest.Repo)] {
			res.notes = append(res.notes, fmt.Sprintf("%s: this agent has no GitHub access to the repository, so its work is not saved", w.Manifest.Repo))
			continue
		}
		w.Manifest.Skipped = capNotes(skipped)
		packed, err := savedwork.Pack(w)
		if err != nil {
			res.notes = append(res.notes, fmt.Sprintf("%s: %v", w.Manifest.Repo, err))
			continue
		}
		if key == nil {
			if key, err = r.p.cfg.SavedWorkKey(); err != nil || len(key) == 0 {
				return lost("the upload key is not available: %v", err)
			}
		}
		// One upload at a time per koinon-agentd (and per koinon-api): a bundle is up to 50 MB.
		select {
		case r.p.saveSem <- struct{}{}:
		case <-sctx.Done():
			return lost("saving waited longer than %s for another save", r.p.cfg.SaveTimeout)
		}
		up, err := savedwork.Upload(sctx, r.p.http, r.p.cfg.SavedWorkURL, key, r.run.OrgID.String(), r.run.ID.String(), r.run.Epoch, len(res.rows)+1, packed)
		<-r.p.saveSem
		if err != nil {
			res.notes = append(res.notes, fmt.Sprintf("%s: storing the work failed: %v", w.Manifest.Repo, err))
			continue
		}
		var id pgtype.UUID
		_ = id.Scan(up.SavedWorkID)
		res.rows = append(res.rows, savedRow{id: id, approvalID: up.ApprovalID, branch: up.Branch})
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
	if _, b, ok := providers.Default().Binding(r.vcfg); ok {
		for _, t := range b.Targets {
			out[strings.ToLower(t)] = true
		}
	}
	return out
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
				if int64(len(out)) < size+1 || out[size] != '\n' {
					return nil, nil, errors.New("the output ends inside a file or a file is not followed by a newline")
				}
				cur.Content[file.Path] = out[:size:size]
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
