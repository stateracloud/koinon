// SPDX-License-Identifier: Apache-2.0

// Command adr is the tool for docs/adr (issue 283, ADR 0046, ADR 0118). It reads the YAML front matter
// of every ADR and works offline.
//
//	adr lint [dir]                  check the ADRs and that the generated files are current
//	adr index [--check] [dir]       write (or check) README.md's index table, adrs.json and graph.md
//	adr find [flags] [words...]     list ADRs; --area, --tag, --status filter, --text searches the body
//	adr show [--body] NNNN          one ADR with its links in both directions
//	adr graph [--relates] [--depth N] [NNNN]
//	                                Mermaid graph of amends and supersedes links (around NNNN)
//
// The ADR directory is $ADR_ROOT/docs/adr; ADR_ROOT defaults to the working directory. hack/adr,
// hack/adr-lint.sh and hack/adr-index.sh are the wrappers. Exit codes: 0 clean, 1 findings, 2 usage or
// read error.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	code, err := run(os.Args[1:], os.Stdout, os.Stderr)
	if err != nil {
		pln(os.Stderr, "adr:", err)
	}
	os.Exit(code)
}

func adrDir(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	root := os.Getenv("ADR_ROOT")
	if root == "" {
		root = "."
	}
	return filepath.Join(root, "docs", "adr")
}

func usage(stderr io.Writer) (int, error) {
	pln(stderr, "usage: adr lint|index|find|show|graph [flags] (see hack/adr --help)")
	return 2, nil
}

func run(args []string, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		return usage(stderr)
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "index: fail when a generated file is stale")
	area := fs.String("area", "", "find: only this area")
	tag := fs.String("tag", "", "find: only this tag")
	status := fs.String("status", "", "find: only this status")
	text := fs.Bool("text", false, "find: also search the body")
	body := fs.Bool("body", false, "show: print the whole file")
	relates := fs.Bool("relates", false, "graph: draw relates links too")
	depth := fs.Int("depth", 1, "graph: links to follow around NNNN")
	if err := fs.Parse(reorder(rest)); err != nil {
		return 2, nil
	}
	pos := fs.Args()
	switch cmd {
	case "lint", "index":
		dir := adrDir(pos)
		if cmd == "lint" {
			return runLint(dir, stdout, stderr)
		}
		return runIndex(dir, *check, stdout, stderr)
	case "find", "show", "graph":
		adrs, _, err := loadADRs(adrDir(nil))
		if err != nil {
			return 2, err
		}
		switch cmd {
		case "find":
			return runFind(adrs, pos, *area, *tag, *status, *text, stdout)
		case "show":
			return runShow(adrs, pos, *body, stdout, stderr)
		default:
			return runGraph(adrs, pos, *relates, *depth, stdout, stderr)
		}
	}
	return usage(stderr)
}

// reorder moves flags in front of words, so "adr find leak --area x" works as users expect.
func reorder(args []string) []string {
	var flags, words []string
	valued := map[string]bool{"--area": true, "-area": true, "--tag": true, "-tag": true, "--status": true, "-status": true, "--depth": true, "-depth": true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && !strings.Contains(a, "=") && valued[a] && i+1 < len(args) {
			flags = append(flags, a, args[i+1])
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
		} else {
			words = append(words, a)
		}
	}
	return append(flags, words...)
}

func runLint(dir string, stdout, stderr io.Writer) (int, error) {
	errs, warns, err := lint(dir)
	if err != nil {
		return 2, err
	}
	adrs, _, err := loadADRs(dir)
	if err != nil {
		return 2, err
	}
	if len(errs) == 0 {
		stale, err := writeOutputs(dir, adrs, true)
		if err != nil {
			return 2, err
		}
		for _, n := range stale {
			errs = append(errs, fmt.Sprintf("docs/adr/%s is stale; run hack/adr-index.sh and commit the result", n))
		}
	}
	for _, w := range warns {
		pln(stderr, "adr-lint: warning:", w)
	}
	for _, e := range errs {
		pln(stderr, "adr-lint:", e)
	}
	if len(errs) > 0 {
		return 1, nil
	}
	pf(stdout, "adr-lint: %d ADRs, metadata valid, links both ways, README index, adrs.json and graph.md current\n", len(adrs))
	return 0, nil
}

func runIndex(dir string, check bool, stdout, stderr io.Writer) (int, error) {
	adrs, _, err := loadADRs(dir)
	if err != nil {
		return 2, err
	}
	// Read the front matter before writing outputs that carry it: a broken ADR must not land in adrs.json.
	if errs, _, err := lint(dir); err != nil {
		return 2, err
	} else if len(errs) > 0 {
		for _, e := range errs {
			pln(stderr, "adr-index:", e)
		}
		return 1, nil
	}
	stale, err := writeOutputs(dir, adrs, check)
	if err != nil {
		return 2, err
	}
	if check && len(stale) > 0 {
		pf(stderr, "adr-index: %s stale; run hack/adr-index.sh and commit the result\n", strings.Join(stale, ", "))
		return 1, nil
	}
	if check {
		pln(stdout, "adr-index: README index, adrs.json and graph.md are current")
	} else {
		pf(stdout, "adr-index: wrote README.md, adrs.json and graph.md in %s\n", dir)
	}
	return 0, nil
}

// Output helpers: a failed write to stdout or stderr has no better handler than the exit code.
func pln(w io.Writer, a ...any)          { _, _ = fmt.Fprintln(w, a...) }
func pf(w io.Writer, f string, a ...any) { _, _ = fmt.Fprintf(w, f, a...) }
func pp(w io.Writer, a ...any)           { _, _ = fmt.Fprint(w, a...) }
