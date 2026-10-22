// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// instructionsPath is where a flavor reads Koinon's instructions for the run (for OpenCode,
// the global AGENTS.md links to it; flavors/opencode/README.md, ADR 0052). The agent can write
// the home, so agentd writes the file before every harness start and restores it before every
// prompt.
const instructionsPath = "/sandbox/.koinon/AGENTS.md"

// instructionsPreamble opens the file, ahead of the agent version's instructions.
const instructionsPreamble = "# Koinon\n\n" +
	"These are your instructions from Koinon for this run. Koinon writes this file before the " +
	"run and restores it before every prompt, so changing it has no effect.\n\n"

// writeScript replaces the file in one rename, so a harness never reads a half-written file;
// umask 077 keeps it and its directory private to the sandbox user. $1 is the directory, $2
// the file; the content arrives on stdin.
const writeScript = `umask 077 && mkdir -p "$1" && t=$(mktemp "$1/.AGENTS.md.XXXXXX") && cat > "$t" && mv -f "$t" "$2"`

// readScript prints the file if it is a regular file, not a link.
const readScript = `test -f "$1" && test ! -L "$1" && exec cat "$1"`

// instructionsFile is the file's content for a version's instructions. It is never empty,
// which the flavors' entry scripts require.
func instructionsFile(instructions string) []byte {
	if instructions = strings.TrimSpace(instructions); instructions == "" {
		return []byte(instructionsPreamble + "The agent version has no instructions of its own.\n")
	}
	return []byte(instructionsPreamble + instructions + "\n")
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
	return nil
}

// checkInstructions restores the instruction file if its hash is not content's or it is
// gone, and records that it did.
func (r *runner) checkInstructions(ctx context.Context, workspace, sandbox string, content []byte) error {
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

func dirOf(path string) string { return path[:strings.LastIndexByte(path, '/')] }
