// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/procoduck/koinon/internal/db/store"
)

// instructionsPath is where a flavor reads Koinon's instructions for the run (for OpenCode,
// the global AGENTS.md links to it; flavors/opencode/README.md, ADR 0052). The agent can write
// the home, so agentd writes the file before every harness start and restores it before every
// prompt.
const instructionsPath = "/sandbox/.koinon/AGENTS.md"

// instructionLink is the path a flavor's harness reads its instructions from: a symbolic link
// to instructionsPath. agentd checks it with the file before every prompt and puts it back if
// the agent changed it. A link in the image's read-only file system cannot change; one in the
// home can.
func instructionLink(f store.AgentFlavor) string {
	switch f {
	case store.AgentFlavorOpencode:
		// The global config directory's AGENTS.md (flavors/opencode/Dockerfile).
		return "/etc/koinon/opencode/AGENTS.md"
	default:
		return ""
	}
}

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

// linkScript makes $1 a symbolic link to $2. It exits 0 when it already is, and 3 when it
// replaced what was there, printing the old link's target (nothing if $1 was no link).
const linkScript = `t=$(readlink -- "$1") && [ "$t" = "$2" ] && exit 0; ` +
	`mkdir -p -- "$(dirname -- "$1")" && rm -rf -- "$1" && ln -s -- "$2" "$1" && printf '%s' "$t" && exit 3`

// versionInstructions returns the instructions an agent version's config holds
// ({"instructions": "..."}, as agentconfig.Config writes them).
func versionInstructions(config []byte) string {
	var c struct {
		Instructions string `json:"instructions"`
	}
	_ = json.Unmarshal(config, &c)
	return strings.TrimSpace(c.Instructions)
}

// instructionsFile is the file's content for a version's instructions. It is never empty,
// which the flavors' entry scripts require.
func instructionsFile(instructions string) []byte {
	if instructions == "" {
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
