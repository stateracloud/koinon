// SPDX-License-Identifier: Apache-2.0

// Command generatecheck is the first step of `make generate` (issue 1169). It checks the tools that
// the Go toolchain cannot supply (node, and the protoc-gen-es plugin that `pnpm install` puts in
// web/node_modules). buf, sqlc, oapi-codegen and the Go plugins come from `go tool`. For each missing
// tool it prints one line that names the command that installs it, and it exits 1.
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// esPlugin is the protoc-gen-es script, relative to the repository root. The buf.gen*.yaml files run
// it with node, so the generate step does not depend on the node_modules/.bin links.
const esPlugin = "web/node_modules/@bufbuild/protoc-gen-es/bin/protoc-gen-es"

// missing returns one line for each tool that is not present under root.
func missing(root string, lookPath func(string) (string, error)) []string {
	var lines []string
	if _, err := lookPath("node"); err != nil {
		lines = append(lines, "generate: node is missing; install Node 22 or newer (https://nodejs.org), then run: corepack enable")
	}
	if _, err := os.Stat(filepath.Join(root, esPlugin)); err != nil { //nolint:gosec // root is the repository root the caller names; the tool reads no untrusted input
		lines = append(lines, "generate: protoc-gen-es is missing; run: pnpm --dir web install --frozen-lockfile")
	}
	return lines
}

func run(root string, lookPath func(string) (string, error), stderr io.Writer) int {
	lines := missing(root, lookPath)
	for _, l := range lines {
		_, _ = fmt.Fprintln(stderr, l)
	}
	if len(lines) > 0 {
		return 1
	}
	return 0
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	os.Exit(run(root, exec.LookPath, os.Stderr))
}
