// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/flavor"
)

// instructionsPath is where a flavor reads Koinon's instructions for the run (for OpenCode,
// the "instructions" key of its config names it; flavors/opencode/README.md, ADR 0052). The agent can write
// the home, so agentd writes the file before every harness start and restores it before every
// prompt.
const instructionsPath = "/sandbox/.koinon/AGENTS.md"

// instructionLink is the path a flavor's harness reads its instructions from: a symbolic link
// to instructionsPath. agentd checks it with the file before every prompt and puts it back if
// the agent changed it. A link in the image's read-only file system cannot change; one in the
// home can.
func instructionLink(f store.AgentFlavor) string {
	d, _ := flavor.Lookup(f)
	return d.InstructionLink
}

// instructionsPreamble opens the file, ahead of the agent version's instructions.
const instructionsPreamble = "# Koinon\n\n" +
	"These are your instructions from Koinon for this run. Koinon writes this file before the " +
	"run and restores it before every prompt, so changing it has no effect.\n\n"

// workingWithData is Koinon's guidance on the tools the flavor image ships (flavors/opencode/
// README.md) and on how to read large inputs. It follows the preamble in every run, whatever the
// agent version says. OpenCode cuts a tool result at 2,000 lines or 50 KiB and saves the full
// output to a file; the section tells the agent to query that file rather than run the command
// again. The image has no network, no pip and no package manager the agent may use.
const workingWithData = "## Working with large data\n\n" +
	"- Check size before reading: `wc -c FILE`, `ls -l`, `file FILE`. Read in ranges, not whole files.\n" +
	"- Search with `rg` (use `-n`, `-c`, `-l`, `--max-count`), find files with `fd`, search code " +
	"structure with `ast-grep --lang LANG -p PATTERN`. Limit output with `| head -50`.\n" +
	"- JSON and JSONL: `jq`; YAML, XML, TOML and CSV: `yq` (`yq -p xml -o json f.xml | jq ...`). " +
	"Select fields; do not `cat` a large file.\n" +
	"- CSV, TSV, JSON and Parquet tables: `duckdb -c \"DESCRIBE SELECT * FROM 'f.csv'\"`, then " +
	"`SUMMARIZE`, `SELECT count(*)` and targeted SQL with `LIMIT`. Use `-json` or `-csv` for " +
	"machine-readable output.\n" +
	"- SQLite files: `sqlite3 -readonly FILE '.schema'`, then targeted queries.\n" +
	"- PDFs: `pdfinfo FILE`, then `pdftotext -layout -f N -l M FILE - | head -200`. Do not open a " +
	"PDF with the read tool.\n" +
	"- Git: `git diff --stat` before `git diff -- PATH`; `git log --oneline -n 20`.\n" +
	"- When a tool result says `full output saved to FILE`, query that file with `rg`, `jq` or " +
	"`duckdb`; do not run the command again.\n" +
	"- `python3` (also `python`) has the standard library only: json, csv, sqlite3, re, gzip, " +
	"zipfile, statistics, decimal. There is no pip and no network. Write scratch scripts under " +
	"`/tmp`, not in the repository, and print counts and samples, not whole datasets.\n" +
	"- Never try to install anything (`pip`, `npm`, `apt`, `curl | sh`); it cannot work here. " +
	"Use what is installed, or say what is missing.\n\n"

// writeScript replaces the file in one rename, so a harness never reads a half-written file;
// umask 077 keeps it and its directory private to the sandbox user. $1 is the directory, $2
// the file; the content arrives on stdin.
//
// Whatever the agent put at $2 goes first, a directory or a link included: mv would move the
// file into a directory, or through a link. writeInstructions reads the file back and checks
// its hash, which catches a directory the agent recreates between rm and mv.
const writeScript = `umask 077 && mkdir -p "$1" && t=$(mktemp "$1/.AGENTS.md.XXXXXX") && cat > "$t" && ` +
	`rm -rf -- "$2" && mv -f -- "$t" "$2"`

// readScript prints the file if it is a regular file, not a link.
const readScript = `test -f "$1" && test ! -L "$1" && exec cat "$1"`

// linkScript makes $1 a symbolic link to $2. It exits 0 when it already is, and 3 when it
// replaced what was there, printing the old link's target (nothing if $1 was no link). It
// removes whatever is at $1 first, and checks the new link before it reports the restore: ln
// would put the link inside a directory recreated meanwhile.
const linkScript = `t=$(readlink -- "$1") && [ "$t" = "$2" ] && exit 0; ` +
	`mkdir -p -- "$(dirname -- "$1")" && rm -rf -- "$1" && ln -s -- "$2" "$1" && ` +
	`[ "$(readlink -- "$1")" = "$2" ] && printf '%s' "$t" && exit 3`

// instructionsFile is the file's content for a version's instructions. It is never empty,
// which the flavors' entry scripts require.
func instructionsFile(instructions string) []byte {
	if instructions = strings.TrimSpace(instructions); instructions == "" {
		return []byte(instructionsPreamble + workingWithData + "The agent version has no instructions of its own.\n")
	}
	return []byte(instructionsPreamble + workingWithData + instructions + "\n")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeInstructions writes content to instructionsPath in the sandbox. Any failure fails the
// run: a harness must not start on stale or missing instructions.
func (r *runner) writeInstructions(ctx context.Context, workspace, sandbox string, content []byte) error {
	_, code, stderr, err := r.sandboxExec(ctx, workspace, sandbox,
		[]string{"/bin/sh", "-c", writeScript, "sh", dirOf(instructionsPath), instructionsPath}, content)
	if err != nil {
		return err
	}
	if code != 0 {
		return &failure{reason: fmt.Sprintf("write the instructions to %s: exit %d: %s", instructionsPath, code, strings.TrimSpace(stderr))}
	}
	// Only a regular file with this content counts as written.
	got, code, _, err := r.sandboxExec(ctx, workspace, sandbox, []string{"/bin/sh", "-c", readScript, "sh", instructionsPath}, nil)
	if err != nil {
		return err
	}
	if code != 0 || sha256Hex(got) != sha256Hex(content) {
		return &failure{reason: fmt.Sprintf("write the instructions to %s: the path does not hold the file agentd wrote", instructionsPath)}
	}
	return nil
}

// checkInstructions restores the instruction file if its hash is not content's or it is
// gone, and the flavor's link to it if the link changed, and records each restore.
func (r *runner) checkInstructions(ctx context.Context, workspace, sandbox string, flavor store.AgentFlavor, content []byte) error {
	if err := r.checkInstructionFile(ctx, workspace, sandbox, content); err != nil {
		return err
	}
	if link := instructionLink(flavor); link != "" {
		return r.checkInstructionLink(ctx, workspace, sandbox, link, content)
	}
	return nil
}

func (r *runner) checkInstructionFile(ctx context.Context, workspace, sandbox string, content []byte) error {
	got, code, _, err := r.sandboxExec(ctx, workspace, sandbox, []string{"/bin/sh", "-c", readScript, "sh", instructionsPath}, nil)
	if err != nil {
		return err
	}
	if code == 0 && sha256Hex(got) == sha256Hex(content) {
		return nil
	}
	reason, found := "changed", ""
	if code != 0 {
		reason = "missing"
	} else {
		found = sha256Hex(got)
	}
	r.log.Warn("restoring the instruction file", "reason", reason)
	if err := r.writeInstructions(ctx, workspace, sandbox, content); err != nil {
		return err
	}
	payload := map[string]any{"path": instructionsPath, "reason": reason, "sha256": sha256Hex(content)}
	if found != "" {
		payload["foundSha256"] = found
	}
	return r.record(ctx, "run.instructions_restored", payload)
}

// checkInstructionLink restores link, the flavor's path to the instruction file, if it is no
// longer a symbolic link to it. A link that cannot be restored fails the run.
func (r *runner) checkInstructionLink(ctx context.Context, workspace, sandbox, link string, content []byte) error {
	found, code, stderr, err := r.sandboxExec(ctx, workspace, sandbox,
		[]string{"/bin/sh", "-c", linkScript, "sh", link, instructionsPath}, nil)
	switch {
	case err != nil:
		return err
	case code == 0:
		return nil
	case code != 3:
		return &failure{reason: fmt.Sprintf("restore the instruction link %s: exit %d: %s", link, code, strings.TrimSpace(stderr))}
	}
	reason := "changed"
	if len(found) == 0 {
		reason = "missing"
	}
	r.log.Warn("restored the instruction link", "path", link, "reason", reason)
	payload := map[string]any{"path": link, "reason": reason, "target": instructionsPath, "sha256": sha256Hex(content)}
	if len(found) > 0 {
		payload["foundTarget"] = string(found)
	}
	return r.record(ctx, "run.instructions_restored", payload)
}

func dirOf(path string) string { return path[:strings.LastIndexByte(path, '/')] }
