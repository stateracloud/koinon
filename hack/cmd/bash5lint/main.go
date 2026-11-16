// SPDX-License-Identifier: Apache-2.0

// Command bash5lint checks the bash 5 guard (ADR 0090, old issue 553): every `#!/usr/bin/env bash`
// script carries the guard line
//
//	[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 ..." >&2; exit 2; }
//
// as its first command: the first line that is neither blank nor a comment. It must come first because
// an old bash (macOS ships 3.2) parses the file line by line and has to reach the guard before any
// bash 4+ syntax, and the guard itself uses only POSIX syntax. The lint checks the exact text, so the
// message cannot drift between scripts.
//
// Hooks: the registered hack/hooks/*-check.sh start with the maintainer opt-in line (hookGate, issue
// 843; POSIX, so bash 3.2 reaches it) and carry the fail-open variant of the guard (hookGuard) as their
// second command. Any other bash shebang (#!/bin/bash, env -S bash, ...) is rejected.
//
// Scope: every *.sh file under the root whose first line is `#!/usr/bin/env bash`. Not in scope, by
// design (see ADR 0090): files without a bash shebang (libraries that are only sourced, such as
// hack/chart-check.d/*.sh, which a guarded script loads) and scripts with a #!/bin/sh shebang (they run
// in container images with no bash).
//
// Usage: bash5lint ROOT. hack/bash5-lint.sh is the wrapper that passes the repository root.
// Exit codes: 0 clean, 1 findings, 2 usage or read error (ADR 0118).
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const (
	shebangBash = "#!/usr/bin/env bash"

	guard = `[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }`

	// PreToolUse hooks (hack/hooks/*-check.sh) skip with a message instead: exit 2 would block the tool call.
	hookGuard = `[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo '{"systemMessage":"Koinon hooks skipped: they need bash 5 or newer; on macOS: brew install bash"}'; exit 0; }`
	// Maintainer hooks do nothing unless KOINON_MAINTAINER_HOOKS=1 (docs/maintainers/hooks.md); they drain stdin first.
	hookGate = `[ "${KOINON_MAINTAINER_HOOKS:-}" = 1 ] || { cat >/dev/null; exit 0; }`
)

// registeredHooks is exactly the registered PreToolUse hooks. Add a new hook here when
// .claude/settings.json registers it.
var registeredHooks = []string{
	"hack/hooks/agent-spawn-check.sh",
	"hack/hooks/bash-read-check.sh",
	"hack/hooks/read-result-check.sh",
	"hack/hooks/sleep-chain-check.sh",
	"hack/hooks/codegraph-prompt-check.sh",
	"hack/hooks/context-cap-check.sh",
	"hack/hooks/grilling-question-check.sh",
	"hack/hooks/prose-question-stop-check.sh",
	"hack/hooks/question-checklist-check.sh",
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: bash5lint ROOT")
		os.Exit(2)
	}
	code, err := run(os.Args[1], os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bash5-lint:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

// run lints the tree under root. It returns 1 when a script breaks the rule.
func run(root string, stdout, stderr io.Writer) (int, error) {
	files, err := listScripts(root)
	if err != nil {
		return 0, err
	}
	failed := false
	n := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return 0, err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(f, root+string(filepath.Separator)))
		msg, counted := checkScript(rel, string(data))
		if counted {
			n++
		}
		if msg != "" {
			fmt.Fprintf(stderr, "bash5-lint: %s: %s\n", rel, msg)
			failed = true
		}
	}
	if failed {
		fmt.Fprintln(stderr, "bash5-lint: put this line before the first command of each script listed (ADR 0090):")
		fmt.Fprintln(stderr, guard)
		return 1, nil
	}
	fmt.Fprintf(stdout, "bash5-lint: %d scripts, every one starts with the bash 5 guard\n", n)
	return 0, nil
}

// checkScript returns the complaint about one script ("" when it is fine) and whether the script
// counts as a bash script of the scope.
func checkScript(rel, content string) (msg string, counted bool) {
	lines := strings.Split(content, "\n")
	shebang := lines[0]
	if strings.HasPrefix(shebang, "#!") && strings.Contains(shebang, "bash") && shebang != shebangBash {
		return fmt.Sprintf("bash shebang '%s' (use %s)", shebang, shebangBash), false
	}
	if shebang != shebangBash {
		return "", false
	}
	first, second := commands(lines[1:])
	if slices.Contains(registeredHooks, rel) {
		// hooks fail open: exit 2 would block the tool call (ADR 0090, point 6)
		if first != hookGate || second != hookGuard {
			return "a hook starts with the KOINON_MAINTAINER_HOOKS line, then the fail-open bash 5 guard", true
		}
		return "", true
	}
	if first != guard {
		return "the first command is not the bash 5 guard", true
	}
	return "", true
}

// commands returns the first two lines that are neither blank nor a comment.
func commands(lines []string) (first, second string) {
	var found []string
	for _, l := range lines {
		if len(strings.Fields(l)) == 0 || strings.HasPrefix(strings.TrimLeft(l, " \t"), "#") {
			continue
		}
		found = append(found, l)
		if len(found) == 2 {
			break
		}
	}
	if len(found) > 0 {
		first = found[0]
	}
	if len(found) > 1 {
		second = found[1]
	}
	return first, second
}

// listScripts returns the absolute paths of the *.sh files under root, sorted. In a git checkout
// they come from git (tracked and untracked, minus ignored); elsewhere from a walk that skips
// .git and node_modules.
func listScripts(root string) ([]string, error) {
	var files []string
	if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
		cmd := exec.Command("git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard", "-z", "--", "*.sh")
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("git ls-files: %w: %s", err, strings.TrimSpace(errb.String()))
		}
		for _, p := range strings.Split(out.String(), "\x00") {
			if p != "" {
				files = append(files, filepath.Join(root, p))
			}
		}
	} else {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			if d.Type().IsRegular() && strings.HasSuffix(p, ".sh") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	slices.Sort(files)
	return files, nil
}
