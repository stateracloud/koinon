// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestAdr(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "adr")
}

func write(root, rel, content string) {
	p := filepath.Join(root, rel)
	Expect(os.MkdirAll(filepath.Dir(p), 0o755)).To(Succeed())
	Expect(os.WriteFile(p, []byte(content), 0o644)).To(Succeed())
}

// adrFile renders an ADR with front matter and the matching prose header.
func adrFile(n, title, status, extra, header string) string {
	return "---\nstatus: " + status + "\ndate: 2026-11-01\narea: engineering-process\ntags: [docs]\n" + extra + "---\n# " + n + ". " + title +
		"\n\n- Status: " + strings.ToUpper(status[:1]) + status[1:] + "\n- Date: 2026-11-01\n- Area: engineering-process\n- Tags: docs\n" + header + "\n## Context\n\nText.\n"
}

func tree() string {
	root := GinkgoT().TempDir()
	write(root, "docs/metadata/areas.txt", "engineering-process  CI\n")
	write(root, "docs/metadata/tags.txt", "docs  documents\nci  CI\n")
	write(root, "docs/adr/0000-template.md", "# 0000. Template\n")
	write(root, "docs/adr/README.md", "# ADRs\n\nprose\n\n## Index\n\nold\n")
	write(root, "docs/adr/0001-first.md", adrFile("0001", "First | pipe", "accepted", "", ""))
	write(root, "docs/adr/0002-second.md", adrFile("0002", "Second", "accepted", "", ""))
	return filepath.Join(root, "docs", "adr")
}

func runCmd(dir string, args ...string) (code int, out, errOut string) {
	var o, e bytes.Buffer
	GinkgoT().Setenv("ADR_ROOT", filepath.Join(dir, "..", ".."))
	code, err := run(args, &o, &e)
	Expect(err).NotTo(HaveOccurred())
	return code, o.String(), e.String()
}

func lintCode(dir string) (int, string) {
	code, _, e := runCmd(dir, "lint", dir)
	return code, e
}

var _ = Describe("adr", func() {
	It("generates the index, adrs.json and the graph, and the lint then passes", func() {
		dir := tree()
		code, _ := lintCode(dir)
		Expect(code).To(Equal(1)) // generated files are stale
		code, _, _ = runCmd(dir, "index", dir)
		Expect(code).To(Equal(0))
		code, e := lintCode(dir)
		Expect(code).To(Equal(0), e)
		readme, _ := os.ReadFile(filepath.Join(dir, "README.md"))
		Expect(string(readme)).To(ContainSubstring("| [0001](0001-first.md) | First \\| pipe | Accepted |"))
		Expect(string(readme)).To(ContainSubstring("prose"))
		js, _ := os.ReadFile(filepath.Join(dir, "adrs.json"))
		Expect(string(js)).To(ContainSubstring(`"number": "0002"`))
		Expect(filepath.Join(dir, "graph.md")).To(BeAnExistingFile())
	})

	It("fails on a stale generated file after a new ADR", func() {
		dir := tree()
		runCmd(dir, "index", dir)
		write(dir, "0003-third.md", adrFile("0003", "Third", "accepted", "", ""))
		code, e := lintCode(dir)
		Expect(code).To(Equal(1))
		Expect(e).To(ContainSubstring("stale"))
	})

	It("rejects a missing block, a bad status, area, tag and date", func() {
		dir := tree()
		write(dir, "0003-third.md", "# 0003. Third\n\n- Status: Accepted\n")
		write(dir, "0004-fourth.md", strings.NewReplacer("status: accepted", "status: done", "area: engineering-process", "area: nope", "tags: [docs]", "tags: [nope]", "date: 2026-11-01", "date: soon").Replace(adrFile("0004", "Fourth", "accepted", "", "")))
		code, e := lintCode(dir)
		Expect(code).To(Equal(1))
		for _, want := range []string{"0003-third.md: the front matter block", "status 'done'", "area 'nope'", "tag 'nope'", "date must be YYYY-MM-DD"} {
			Expect(e).To(ContainSubstring(want))
		}
	})

	It("rejects an unknown key and an unquoted number", func() {
		dir := tree()
		write(dir, "0003-third.md", adrFile("0003", "Third", "accepted", "extra: 1\n", ""))
		write(dir, "0004-fourth.md", adrFile("0004", "Fourth", "accepted", "amends: [0001]\n", "- Amends: [0001](0001-first.md)\n"))
		_, e := lintCode(dir)
		Expect(e).To(ContainSubstring("0003-third.md: front matter"))
		Expect(e).To(ContainSubstring("is not quoted"))
	})

	It("needs the older ADR to name the newer one, and derives amended by", func() {
		dir := tree()
		write(dir, "0003-third.md", adrFile("0003", "Third", "accepted", "amends: [\"0001\"]\n", "- Amends: [0001](0001-first.md)\n"))
		code, e := lintCode(dir)
		Expect(code).To(Equal(1))
		Expect(e).To(ContainSubstring("ADR 0003 amends or supersedes ADR 0001"))
		write(dir, "0001-first.md", strings.Replace(adrFile("0001", "First | pipe", "accepted", "", ""), "- Status: Accepted", "- Status: Accepted; amended by [0003](0003-third.md)", 1))
		runCmd(dir, "index", dir)
		code, e = lintCode(dir)
		Expect(code).To(Equal(0), e)
		_, out, _ := runCmd(dir, "show", "1")
		Expect(out).To(ContainSubstring("amended by:"))
		Expect(out).To(ContainSubstring("0003 Third"))
	})

	It("fails when the prose header and the front matter disagree", func() {
		dir := tree()
		write(dir, "0003-third.md", adrFile("0003", "Third", "accepted", "relates: [\"0001\"]\n", ""))
		_, e := lintCode(dir)
		Expect(e).To(ContainSubstring("front matter relates"))
	})

	It("rejects Obsidian-only syntax outside code", func() {
		dir := tree()
		bad := adrFile("0003", "Third", "accepted", "", "") + "\nSee [[0001-first]].\n\n> [!note]\n> hi\n\n```dataview\nTABLE x\n```\n"
		write(dir, "0003-third.md", bad)
		write(dir, "0004-fourth.md", adrFile("0004", "Fourth", "accepted", "", "")+"\nQuote `[[x]]` and\n\n```\n[[y]]\n```\n")
		_, e := lintCode(dir)
		Expect(e).To(ContainSubstring("0003-third.md:"))
		Expect(e).To(ContainSubstring("wiki link"))
		Expect(e).To(ContainSubstring("callout"))
		Expect(e).To(ContainSubstring("Dataview"))
		Expect(e).NotTo(ContainSubstring("0004-fourth.md"))
	})

	It("finds ADRs by words, area, tag and status, and shows the graph", func() {
		dir := tree()
		_, out, _ := runCmd(dir, "find", "second")
		Expect(out).To(ContainSubstring("0002"))
		Expect(out).NotTo(ContainSubstring("0001"))
		code, _, _ := runCmd(dir, "find", "--tag", "ci")
		Expect(code).To(Equal(1))
		_, out, _ = runCmd(dir, "find", "--status", "accepted", "--area", "engineering-process")
		Expect(strings.Count(out, "\n")).To(Equal(2))
		_, out, _ = runCmd(dir, "graph")
		Expect(out).To(HavePrefix("flowchart LR"))
	})
	It("rejects a duplicate number, a missing link file and a link with the wrong number", func() {
		dir := tree()
		write(dir, "0002-other.md", adrFile("0002", "Other", "accepted", "", ""))
		write(dir, "0003-third.md", adrFile("0003", "Third", "accepted", "relates: [\"0001\"]\n", "- Relates to: [0001](0001-gone.md)\n"))
		write(dir, "0004-fourth.md", adrFile("0004", "Fourth", "accepted", "relates: [\"0001\"]\n", "- Relates to: [0001](0002-second.md)\n"))
		_, e := lintCode(dir)
		Expect(e).To(ContainSubstring("number 0002 is used by more than one file"))
		Expect(e).To(ContainSubstring("0001-gone.md does not exist"))
		Expect(e).To(ContainSubstring("the text says 0001, the file is 0002"))
	})

	It("ignores a number in a parenthetical and joins continuation lines", func() {
		dir := tree()
		hdr := "- Relates to: (see ADR 0002) [0001](0001-first.md),\n  [0002](0002-second.md)\n"
		write(dir, "0003-third.md", adrFile("0003", "Third", "accepted", "relates: [\"0001\", \"0002\"]\n", hdr))
		runCmd(dir, "index", dir)
		code, e := lintCode(dir)
		Expect(code).To(Equal(0), e)
	})

	It("warns about a Proposed ADR without failing", func() {
		dir := tree()
		write(dir, "0003-third.md", adrFile("0003", "Third", "proposed", "", ""))
		runCmd(dir, "index", dir)
		code, e := lintCode(dir)
		Expect(code).To(Equal(0))
		Expect(e).To(ContainSubstring("ADR 0003 is still Proposed"))
	})

	It("checks decides against the Decision line and refuses a Reduces line", func() {
		dir := tree()
		write(dir, "0003-third.md", adrFile("0003", "Third", "accepted", "decides: design.md#9\n", "- Decision: [design.md#2](../design.md)\n- Reduces: [0001](0001-first.md)\n"))
		_, e := lintCode(dir)
		Expect(e).To(ContainSubstring("decides 'design.md#9' does not match"))
		Expect(e).To(ContainSubstring("'- Reduces:' line is not supported"))
	})

	It("matches fences by character and length, and reports file line numbers", func() {
		dir := tree()
		fenced := adrFile("0003", "Third", "accepted", "", "") + "\n~~~\n```\n~~~\n[[x]]\n\n````\n```\n[[y]]\n````\n"
		write(dir, "0003-third.md", fenced)
		_, e := lintCode(dir)
		Expect(e).To(ContainSubstring("0003-third.md:"))
		Expect(strings.Count(e, "wiki link")).To(Equal(1))
		line := 1 + strings.Count(fenced[:strings.Index(fenced, "[[x]]")], "\n")
		Expect(e).To(ContainSubstring(fmt.Sprintf("0003-third.md:%d:", line)))
	})
})
