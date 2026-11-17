// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var dateRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

// readList reads docs/metadata/areas.txt or tags.txt: the first word of each non-comment line.
func readList(path string) (map[string]bool, error) {
	f, err := os.Open(path) //nolint:gosec // path derives from the operator argument
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[strings.Fields(line)[0]] = true
	}
	return out, sc.Err()
}

// lint returns the failures and the warnings for the ADR directory dir. Areas and tags come from
// ../metadata of dir (ADR 0115).
func lint(dir string) (errs, warns []string, err error) {
	adrs, template, err := loadADRs(dir)
	if err != nil {
		return nil, nil, err
	}
	fail := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	if template == nil {
		fail("0000-template.md is missing")
	}
	if len(adrs) == 0 {
		fail("no ADR files in %s", dir)
		return errs, warns, nil
	}
	areas, err := readList(filepath.Join(dir, "..", "metadata", "areas.txt"))
	if err != nil {
		return nil, nil, err
	}
	tags, err := readList(filepath.Join(dir, "..", "metadata", "tags.txt"))
	if err != nil {
		return nil, nil, err
	}
	have := map[string]*ADR{}
	files := map[string]bool{}
	for _, a := range adrs {
		if prev, dup := have[a.Number]; dup {
			fail("number %s is used by more than one file (%s, %s)", a.Number, prev.File, a.File)
		}
		have[a.Number] = a
		files[a.File] = true
	}
	for _, a := range adrs {
		lintOne(a, have, files, areas, tags, fail)
		if strings.HasPrefix(a.Header["Status"], "Proposed") {
			warns = append(warns, fmt.Sprintf("ADR %s is still Proposed; accept or reject it", a.Number))
		}
	}
	lintBackLinks(adrs, have, fail)
	for _, a := range append(slices.Clone(adrs), template) {
		if a != nil {
			lintObsidian(a, fail)
		}
	}
	if readme, err := os.ReadFile(filepath.Join(dir, "README.md")); err == nil { //nolint:gosec // dir is the operator argument
		lintObsidian(&ADR{File: "README.md", Body: string(readme)}, fail)
	}
	return errs, warns, nil
}

func lintOne(a *ADR, have map[string]*ADR, files, areas, tags map[string]bool, fail func(string, ...any)) {
	f := a.File
	if a.Title == "" || !strings.HasPrefix(a.Body, "# "+a.Number+". ") {
		fail("%s: the first line after the front matter must be '# %s. Title'", f, a.Number)
	}
	if !a.HasMeta {
		fail("%s: the front matter block (--- ... ---) is missing", f)
		return
	}
	if a.MetaErr != nil {
		fail("%s: front matter: %v (write ADR numbers as quoted strings, \"0046\")", f, a.MetaErr)
		return
	}
	m := a.Meta
	if !slices.Contains(Statuses, m.Status) {
		fail("%s: status '%s' is not one of %s", f, m.Status, strings.Join(Statuses, ", "))
	}
	if !dateRe.MatchString(m.Date) {
		fail("%s: date must be YYYY-MM-DD (got '%s')", f, m.Date)
	}
	if !areas[m.Area] {
		fail("%s: area '%s' is not in docs/metadata/areas.txt", f, m.Area)
	}
	if n := len(m.Tags); n < 1 || n > 6 {
		fail("%s: needs one to six tags (found %d)", f, n)
	}
	for i, t := range m.Tags {
		if !tags[t] {
			fail("%s: tag '%s' is not in docs/metadata/tags.txt", f, t)
		}
		if slices.Contains(m.Tags[:i], t) {
			fail("%s: tag '%s' is listed twice", f, t)
		}
	}
	for _, l := range []struct {
		name string
		nums []string
	}{{"amends", m.Amends}, {"supersedes", m.Supersedes}, {"relates", m.Relates}} {
		if !slices.IsSorted(l.nums) || len(uniqSorted(l.nums)) != len(l.nums) {
			fail("%s: %s must be sorted and have no duplicates (%v)", f, l.name, l.nums)
		}
		for _, n := range l.nums {
			switch {
			case !numRe.MatchString(n):
				fail("%s: %s: '%s' is not a four-digit ADR number", f, l.name, n)
			case n == a.Number:
				fail("%s: %s names the ADR itself", f, l.name)
			case have[n] == nil:
				fail("%s: %s names ADR %s, but no such ADR file exists", f, l.name, n)
			}
		}
	}
	if m.Decides != "" && !strings.HasPrefix(m.Decides, "design.md") {
		fail("%s: decides must start with design.md (got '%s')", f, m.Decides)
	}
	lintHeader(a, files, fail)
}

// lintHeader checks the prose bullets against the YAML: the prose stays for readers, the YAML is the
// record, and the two must name the same things.
func lintHeader(a *ADR, files map[string]bool, fail func(string, ...any)) {
	f, m := a.File, a.Meta
	st := a.Header["Status"]
	w := statusWord(st)
	switch {
	case st == "":
		fail("%s: no '- Status:' line", f)
	case w == "":
		fail("%s: Status must start with Proposed, Accepted, Deprecated, Superseded by or Rejected (got '%s')", f, st)
	case w != m.Status:
		fail("%s: front matter status '%s' does not match the Status line ('%s')", f, m.Status, st)
	}
	if d := a.Header["Date"]; d != m.Date {
		fail("%s: front matter date '%s' does not match the Date line ('%s')", f, m.Date, d)
	}
	if v := a.Header["Area"]; v != m.Area {
		fail("%s: front matter area '%s' does not match the Area line ('%s')", f, m.Area, v)
	}
	var lineTags []string
	for _, t := range strings.Split(a.Header["Tags"], ",") {
		lineTags = append(lineTags, strings.TrimSpace(t))
	}
	if !slices.Equal(uniqSorted(lineTags), uniqSorted(m.Tags)) {
		fail("%s: front matter tags %v do not match the Tags line (%v)", f, m.Tags, lineTags)
	}
	sup := headerNumbers(a.Header["Supersedes"] + " " + a.Header["Supersedes in part"])
	for _, c := range []struct {
		name string
		want []string
		got  []string
	}{
		{"amends", m.Amends, headerNumbers(a.Header["Amends"])},
		{"supersedes", m.Supersedes, sup},
		{"relates", m.Relates, headerNumbers(a.Header["Relates to"])},
	} {
		if !slices.Equal(c.want, uniqSorted(c.got)) {
			fail("%s: front matter %s %v does not match the header lines (%v)", f, c.name, c.want, uniqSorted(c.got))
		}
	}
	for _, l := range linkRe.FindAllStringSubmatch(a.Header["Status"]+"\n"+a.Header["Relates to"], -1) {
		target, _, _ := strings.Cut(l[2], "#")
		switch {
		case !files[target]:
			fail("%s: link %s: %s does not exist", f, l[0], target)
		case target[:4] != l[1]:
			fail("%s: link %s: the text says %s, the file is %s", f, l[0], l[1], target[:4])
		}
	}
}

// lintBackLinks derives "amended by" and "superseded by" and checks that the header of the older ADR
// names the newer one (Status, "Amended by", "Superseded by" or "Reduced by", ADR 0046 decision 4), and
// that a name there is backed by the newer ADR's front matter.
func lintBackLinks(adrs []*ADR, have map[string]*ADR, fail func(string, ...any)) {
	d := derive(adrs)
	for _, t := range adrs {
		named := linkNumbers(strings.Join([]string{t.Header["Status"], t.Header["Amended by"], t.Header["Superseded by"], t.Header["Reduced by"]}, " "))
		for _, src := range [][]string{d.AmendedBy[t.Number], d.SupersededBy[t.Number]} {
			for _, s := range src {
				if !slices.Contains(named, s) {
					fail("ADR %s amends or supersedes ADR %s, but ADR %s does not name %s in its Status or Amended by / Superseded by line", s, t.Number, t.Number, s)
				}
			}
		}
		// Two ADRs can replace one together (0010 by 0065 and 0066), each through "amends".
		if t.Meta.Status == "superseded" && len(d.SupersededBy[t.Number])+len(d.AmendedBy[t.Number]) == 0 {
			fail("%s: status is superseded, but no ADR lists %s under supersedes or amends", t.File, t.Number)
		}
		claimed := headerNumbers(t.Header["Amended by"] + " " + t.Header["Superseded by"])
		for _, clause := range strings.Split(t.Header["Status"], ";") {
			lc := strings.ToLower(clause)
			if strings.Contains(lc, "amended") || strings.Contains(lc, "superseded") || strings.Contains(lc, "reduced") {
				claimed = append(claimed, linkNumbers(clause)...)
			}
		}
		for _, s := range uniqSorted(claimed) {
			o := have[s]
			if o == nil {
				fail("%s: names ADR %s as its amender, but no such ADR file exists", t.File, s)
				continue
			}
			if !slices.Contains(o.Meta.Amends, t.Number) && !slices.Contains(o.Meta.Supersedes, t.Number) {
				fail("ADR %s says it is amended, superseded or reduced by ADR %s, but ADR %s has no amends or supersedes entry naming %s", t.Number, s, s, t.Number)
			}
		}
	}
}

// lintObsidian rejects editor-only syntax (issue 283): wiki links, callouts and Dataview blocks. Fenced
// code and inline code spans are not read, so an ADR can still quote the syntax.
func lintObsidian(a *ADR, fail func(string, ...any)) {
	inFence := false
	span := regexp.MustCompile("`[^`]*`")
	for i, line := range strings.Split(a.Body, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			info := strings.ToLower(strings.TrimLeft(trim, "`~ "))
			if !inFence && (strings.HasPrefix(info, "dataview") || strings.HasPrefix(info, "dataviewjs")) {
				fail("%s:%d: Dataview block; ADRs use plain Markdown only", a.File, i+1)
			}
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		text := span.ReplaceAllString(line, "")
		switch {
		case strings.Contains(text, "[["):
			fail("%s:%d: '[[' wiki link; use a relative Markdown link", a.File, i+1)
		case regexp.MustCompile(`^\s*>\s*\[![A-Za-z-]+\]`).MatchString(text):
			fail("%s:%d: callout (> [!type]); use a plain blockquote", a.File, i+1)
		}
	}
}
