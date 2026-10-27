// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"path"
	"strings"
)

// HomeDir is where OpenShell mounts the sandbox's workspace volume: the home that is saved.
const HomeDir = "/sandbox"

// excludedNames are directory or file names that are never saved, wherever they sit in the home:
// package-manager and build caches and harness caches. They can be rebuilt, and they dominate a
// home's size (ADR 0032 "Excluded"; the HC1 research, section 3).
var excludedNames = []string{
	"node_modules", ".npm", ".pnpm-store", ".yarn", "__pycache__", ".pytest_cache", ".mypy_cache",
	".ruff_cache", ".gradle", ".m2", ".nuget", ".terraform", ".next", ".turbo", ".parcel-cache",
	".nx",
}

// excludedPaths are paths relative to the home that are never saved.
var excludedPaths = []string{
	".cache", ".bun/install/cache", ".cargo/registry", ".cargo/git", ".local/share/pnpm",
	".local/share/uv", ".local/share/Trash", "go/pkg/mod", "go/pkg/sumdb", ".opencode/cache", ".claude/cache",
	".claude/statsig", ".claude/shell-snapshots", ".codex/cache", ".config/Code/Cache", "tmp",
}

// Excluded reports whether the path (relative to the home, with or without a leading "./") is not
// saved.
func Excluded(name string) bool {
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	if name == "" {
		return false
	}
	parts := strings.Split(name, "/")
	for _, p := range parts {
		for _, e := range excludedNames {
			if p == e {
				return true
			}
		}
	}
	for _, e := range excludedPaths {
		if name == e || strings.HasPrefix(name, e+"/") {
			return true
		}
	}
	return false
}

// TarExcludeArgs are the tar options that leave the excluded paths out on the sandbox side, so
// they are not sent at all.
func TarExcludeArgs() []string {
	var out []string
	for _, e := range excludedNames {
		out = append(out, "--exclude="+e)
	}
	for _, e := range excludedPaths {
		out = append(out, "--exclude=./"+e)
	}
	return out
}
