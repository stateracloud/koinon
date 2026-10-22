// SPDX-License-Identifier: Apache-2.0

package githubtools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits on create_pull_request (ADR 0049, plan item C3).
const (
	MaxFiles      = 100
	MaxTotalBytes = 1 << 20 // 1 MiB of file content
	MaxPathBytes  = 1024
	MaxTitleRunes = 256
	// MaxBodyBytes is GitHub's limit on pull request and comment bodies.
	MaxBodyBytes = 65536
	// BranchPrefix is the only namespace Koinon creates branches in, so an
	// approved call can never write to an existing branch such as main.
	BranchPrefix = "koinon/"
)

// PullRequestArgs are create_pull_request's arguments: the exact change the
// approver sees and Koinon executes after approval.
type PullRequestArgs struct {
	Branch string       `json:"branch" jsonschema:"the new branch Koinon creates for the change; must start with koinon/, for example koinon/fix-readme-typo"`
	Title  string       `json:"title" jsonschema:"pull request title, one line in the repository's commit style"`
	Body   string       `json:"body,omitempty" jsonschema:"pull request description: what changed, why, how it was checked"`
	Base   string       `json:"base,omitempty" jsonschema:"branch to merge into; empty means the repository's default branch"`
	Files  []FileChange `json:"files" jsonschema:"every changed file with its full new content, and every deleted file"`
}

// FileChange is one file of the change.
type FileChange struct {
	Path    string `json:"path" jsonschema:"path relative to the repository root, for example docs/README.md"`
	Content string `json:"content,omitempty" jsonschema:"the file's full new content (UTF-8 text)"`
	Delete  bool   `json:"delete,omitempty" jsonschema:"true to delete the file; content must then be empty"`
	// Executable is the only mode choice: true writes the file as git mode 100755. Without
	// it a new file is regular (100644) and a modified file keeps its mode on the base
	// branch, so an existing script stays executable. Symbolic links (120000) and
	// submodules (160000) cannot be expressed, so they are never written.
	Executable bool `json:"executable,omitempty" jsonschema:"true to write the file as executable (git mode 100755), for example a new script run as ./scripts/check.sh; otherwise a new file is regular (100644) and a modified file keeps its current mode, so an existing executable script stays executable"`
}

// CommentArgs are comment_on_issue's arguments.
type CommentArgs struct {
	IssueNumber int64  `json:"issue_number" jsonschema:"number of the issue or pull request to comment on"`
	Body        string `json:"body" jsonschema:"the comment, in Markdown"`
}

// StatusArgs are get_approval_status's arguments.
type StatusArgs struct {
	ID string `json:"id" jsonschema:"the id from an approval requested result"`
}

// ValidationError is a refusal with the reason the agent gets as a tool error.
type ValidationError struct{ Reasons []string }

func (e *ValidationError) Error() string {
	return "refused: " + strings.Join(e.Reasons, "; ")
}

func refuse(reasons []string) error {
	if len(reasons) == 0 {
		return nil
	}
	return &ValidationError{Reasons: reasons}
}

var branchRE = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// validRef reports whether name is a branch name git and GitHub accept
// (a conservative subset of git check-ref-format).
func validRef(name string) bool {
	if name == "" || len(name) > 200 || !branchRE.MatchString(name) ||
		strings.Contains(name, "..") || strings.Contains(name, "//") ||
		strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") || strings.HasPrefix(part, "-") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func validLine(s string, maxRunes int) bool {
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxRunes {
		return false
	}
	return !strings.ContainsFunc(s, unicode.IsControl)
}

// workflowDir is refused outright (ADR 0049): a workflow change would run in
// CI with the repository's secrets as soon as the branch exists.
const workflowDir = ".github/workflows/"

// pathProblem returns why p is not a safe repository path, or "".
func pathProblem(p string) string {
	switch {
	case p == "":
		return "a file has an empty path"
	case len(p) > MaxPathBytes:
		return fmt.Sprintf("path %.60q… is longer than %d bytes", p, MaxPathBytes)
	case !utf8.ValidString(p) || strings.ContainsFunc(p, unicode.IsControl):
		return fmt.Sprintf("path %q contains control characters or invalid UTF-8", p)
	case strings.Contains(p, `\`):
		return fmt.Sprintf("path %q contains a backslash; use / as the separator", p)
	case strings.HasPrefix(p, "/"):
		return fmt.Sprintf("path %q is absolute; paths are relative to the repository root", p)
	}
	for _, seg := range strings.Split(p, "/") {
		switch {
		case seg == "..":
			return fmt.Sprintf("path %q leaves the repository (path traversal)", p)
		case seg == "." || seg == "":
			return fmt.Sprintf("path %q is not clean (empty or . segments); write it as %q", p, path.Clean(p))
		case strings.EqualFold(seg, ".git"):
			return fmt.Sprintf("path %q is inside .git", p)
		}
	}
	if path.Clean(p) != p {
		return fmt.Sprintf("path %q is not clean; write it as %q", p, path.Clean(p))
	}
	// Case-insensitive: a checkout on a case-insensitive file system would
	// merge .GitHub/Workflows into .github/workflows.
	if strings.HasPrefix(strings.ToLower(p), workflowDir) {
		return fmt.Sprintf("path %q is under .github/workflows/: Koinon never proposes workflow changes; say on the ticket that the change needs one and stop", p)
	}
	return ""
}

// ValidatePullRequest checks args before anything is recorded. Every problem
// is reported, so the agent can fix them in one go.
func ValidatePullRequest(a PullRequestArgs) error {
	var reasons []string
	if !strings.HasPrefix(a.Branch, BranchPrefix) || len(a.Branch) == len(BranchPrefix) || !validRef(a.Branch) {
		reasons = append(reasons, fmt.Sprintf("branch %q must be a new branch name starting with %s, for example %sfix-typo", a.Branch, BranchPrefix, BranchPrefix))
	}
	if a.Base != "" && !validRef(a.Base) {
		reasons = append(reasons, fmt.Sprintf("base %q is not a valid branch name", a.Base))
	}
	if a.Base != "" && a.Base == a.Branch {
		reasons = append(reasons, "base and branch are the same")
	}
	if !validLine(a.Title, MaxTitleRunes) {
		reasons = append(reasons, fmt.Sprintf("title must be one line of 1 to %d characters", MaxTitleRunes))
	}
	if len(a.Body) > MaxBodyBytes || !utf8.ValidString(a.Body) {
		reasons = append(reasons, fmt.Sprintf("body must be UTF-8 text of at most %d bytes", MaxBodyBytes))
	}
	switch n := len(a.Files); {
	case n == 0:
		reasons = append(reasons, "files is empty: list every changed file")
	case n > MaxFiles:
		reasons = append(reasons, fmt.Sprintf("%d files is more than the limit of %d; split the change or ask on the ticket", n, MaxFiles))
	}
	total := 0
	seen := map[string]bool{}
	for _, f := range a.Files {
		if p := pathProblem(f.Path); p != "" {
			reasons = append(reasons, p)
		}
		if seen[f.Path] {
			reasons = append(reasons, fmt.Sprintf("path %q is listed twice", f.Path))
		}
		seen[f.Path] = true
		if f.Delete && f.Content != "" {
			reasons = append(reasons, fmt.Sprintf("path %q is deleted but has content", f.Path))
		}
		if f.Delete && f.Executable {
			reasons = append(reasons, fmt.Sprintf("path %q is deleted but marked executable", f.Path))
		}
		total += len(f.Content)
	}
	if total > MaxTotalBytes {
		reasons = append(reasons, fmt.Sprintf("the files total %d bytes, more than the limit of %d (1 MiB); split the change or ask on the ticket", total, MaxTotalBytes))
	}
	return refuse(reasons)
}

// ValidateComment checks comment_on_issue's arguments.
func ValidateComment(a CommentArgs) error {
	var reasons []string
	if a.IssueNumber <= 0 {
		reasons = append(reasons, "issue_number must be a positive issue or pull request number")
	}
	if strings.TrimSpace(a.Body) == "" || len(a.Body) > MaxBodyBytes || !utf8.ValidString(a.Body) {
		reasons = append(reasons, fmt.Sprintf("body must be non-empty UTF-8 text of at most %d bytes", MaxBodyBytes))
	}
	return refuse(reasons)
}

// Risk is what the approval card leads with (plan item W2): push-and-run risk
// first, the diff collapsed below. It is derived from the arguments and not
// part of the hash.
type Risk struct {
	Files     int      `json:"files"`
	Bytes     int      `json:"bytes"`
	Deletions []string `json:"deletions,omitempty"`
	// CI lists CI and automation configuration outside .github/workflows/
	// (which is refused): actions, other CI systems, Dependabot and the like.
	CI []string `json:"ci,omitempty"`
	// Scripts lists files that are executed by tooling or people: shell and
	// build scripts, Makefiles, git hooks.
	Scripts []string `json:"scripts,omitempty"`
	// Dependencies lists dependency manifests and lock files.
	Dependencies []string `json:"dependencies,omitempty"`
	// Binary lists files whose content is not text (NUL or other control
	// bytes) or whose name says they are binary.
	Binary []string `json:"binary,omitempty"`
	// Executable lists the files the call marks executable (git mode 100755). It is
	// derived from the arguments alone, so a marked file that already is executable on
	// the base branch is listed too; the approval diff shows each file's mode change. An
	// unmarked file keeps its mode, so it never becomes executable.
	Executable []string `json:"executable,omitempty"`
}

var (
	ciPrefixes = []string{".github/", ".gitlab-ci", ".circleci/", ".buildkite/", ".drone", ".travis", "azure-pipelines", ".azure-pipelines/", "jenkinsfile", ".woodpecker", "bitbucket-pipelines", "cloudbuild", ".tekton/", "renovate.json", ".pre-commit-config"}
	scriptExts = []string{".sh", ".bash", ".zsh", ".ps1", ".bat", ".cmd", ".py", ".rb", ".pl", ".mk"}
	scriptBase = []string{"makefile", "gnumakefile", "justfile", "taskfile.yml", "dockerfile", "containerfile", "tiltfile", "magefile.go", "build.gradle", "build.gradle.kts", "rakefile", "noxfile.py", "tox.ini", "setup.py"}
	depBase    = []string{
		"go.mod", "go.sum", "package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lockb",
		"requirements.txt", "pyproject.toml", "poetry.lock", "pipfile", "pipfile.lock", "uv.lock", "setup.cfg",
		"cargo.toml", "cargo.lock", "gemfile", "gemfile.lock", "composer.json", "composer.lock", "pom.xml",
		"gradle.properties", "settings.gradle", "build.sbt", "mix.exs", "mix.lock", "pubspec.yaml", "pubspec.lock",
		"podfile", "podfile.lock", "package.swift", "flake.nix", "flake.lock", "chart.yaml",
	}
	binaryExts = []string{
		".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".pdf", ".zip", ".gz", ".tgz", ".bz2", ".xz", ".7z",
		".jar", ".war", ".class", ".exe", ".dll", ".so", ".dylib", ".a", ".o", ".wasm", ".bin", ".pyc", ".woff", ".woff2", ".ttf", ".otf",
	}
)

// looksBinary reports content that is not text: a NUL, or another control
// character besides tab, newline, carriage return and form feed.
func looksBinary(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return r == utf8.RuneError || (unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r' && r != '\f')
	})
}

// AssessRisk flags push-and-run risk in a validated change.
func AssessRisk(a PullRequestArgs) Risk {
	r := Risk{Files: len(a.Files)}
	for _, f := range a.Files {
		r.Bytes += len(f.Content)
		lower := strings.ToLower(f.Path)
		base := path.Base(lower)
		ext := path.Ext(lower)
		if f.Delete {
			r.Deletions = append(r.Deletions, f.Path)
		}
		if slices.ContainsFunc(ciPrefixes, func(p string) bool { return strings.HasPrefix(lower, p) }) {
			r.CI = append(r.CI, f.Path)
		}
		if slices.Contains(scriptExts, ext) || slices.Contains(scriptBase, base) || strings.HasPrefix(lower, ".husky/") ||
			strings.HasPrefix(lower, "scripts/") || strings.HasPrefix(lower, "hack/") || strings.HasPrefix(f.Content, "#!") {
			r.Scripts = append(r.Scripts, f.Path)
		}
		if slices.Contains(depBase, base) || strings.HasPrefix(base, "requirements") && ext == ".txt" {
			r.Dependencies = append(r.Dependencies, f.Path)
		}
		if slices.Contains(binaryExts, ext) || looksBinary(f.Content) {
			r.Binary = append(r.Binary, f.Path)
		}
		if f.Executable {
			r.Executable = append(r.Executable, f.Path)
		}
	}
	return r
}

// Canonical returns the canonical JSON of v: the value as encoding/json
// writes it, then re-encoded with object keys sorted, no insignificant
// whitespace and no HTML escaping. Two calls with the same meaning (key
// order, spacing, escapes) give the same bytes, and so the same hash.
func Canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(generic); err != nil { // maps encode with sorted keys
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// canonicalPullRequest normalises the order-insensitive parts (files are a
// set keyed by path) before Canonical.
func canonicalPullRequest(a PullRequestArgs) ([]byte, error) {
	a.Files = slices.Clone(a.Files)
	slices.SortFunc(a.Files, func(x, y FileChange) int { return strings.Compare(x.Path, y.Path) })
	return Canonical(a)
}

var errNoIdentity = errors.New("this tool server answers only calls routed through Koinon's agentgateway, which identifies the agent")
