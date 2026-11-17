// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Meta is the YAML front matter of an ADR (ADR 0046, amended 2026-11-17). Numbers are four-digit
// strings and each list is sorted and unique. "Amended by" and "superseded by" are derived, never written.
type Meta struct {
	Status     string   `yaml:"status" json:"status"`
	Date       string   `yaml:"date" json:"date"`
	Area       string   `yaml:"area" json:"area"`
	Tags       []string `yaml:"tags" json:"tags"`
	Decides    string   `yaml:"decides,omitempty" json:"decides,omitempty"`
	Amends     []string `yaml:"amends,omitempty" json:"amends"`
	Supersedes []string `yaml:"supersedes,omitempty" json:"supersedes"`
	Relates    []string `yaml:"relates,omitempty" json:"relates"`
}

// Statuses are the values of Meta.Status (ADR 0046 decision 4).
var Statuses = []string{"proposed", "accepted", "deprecated", "superseded", "rejected"}

// ADR is one parsed record. Header holds the prose bullets above the first "## " heading.
type ADR struct {
	Number  string            `json:"number"`
	File    string            `json:"file"`
	Title   string            `json:"title"`
	Meta    Meta              `json:"-"`
	HasMeta bool              `json:"-"`
	Header  map[string]string `json:"-"` // first "- Key:" bullet text, continuation lines joined
	Keys    []string          `json:"-"` // bullet keys in file order
	Body    string            `json:"-"` // everything after the front matter
	MetaErr error             `json:"-"`
	Offset  int               `json:"-"` // lines of front matter before Body
}

var (
	fileRe  = regexp.MustCompile(`^([0-9]{4})-.+\.md$`)
	titleRe = regexp.MustCompile(`^# ([0-9]{4})\. +(.+)$`)
	bulletR = regexp.MustCompile(`^- ([A-Za-z][A-Za-z ]*):\s*(.*)$`)
	linkRe  = regexp.MustCompile(`\[([0-9]{4})\]\(([^)]*)\)`)
	adrRe   = regexp.MustCompile(`ADR ([0-9]{4})`)
	parenRe = regexp.MustCompile(`\([^()]*\)`)
	numRe   = regexp.MustCompile(`^[0-9]{4}$`)
)

// splitFrontMatter returns the YAML text and the rest of the file. ok is false without a leading "---".
func splitFrontMatter(src []byte) (front, rest []byte, ok bool) {
	if !bytes.HasPrefix(src, []byte("---\n")) {
		return nil, src, false
	}
	tail := src[4:]
	if bytes.HasPrefix(tail, []byte("---\n")) {
		return nil, tail[4:], true
	}
	i := bytes.Index(tail, []byte("\n---\n"))
	if i < 0 {
		return nil, src, false
	}
	return tail[:i+1], tail[i+5:], true
}

// parseADR reads one ADR file. A front matter error is kept in MetaErr so lint reports it with the file.
func parseADR(dir, name string) (*ADR, error) {
	src, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // dir is the operator argument
	if err != nil {
		return nil, err
	}
	a := &ADR{Number: name[:4], File: name, Header: map[string]string{}}
	front, rest, has := splitFrontMatter(src)
	a.HasMeta = has
	a.Body = string(rest)
	if has {
		a.Offset = bytes.Count(src[:len(src)-len(rest)], []byte("\n"))
		if err := checkQuoted(front); err != nil {
			a.MetaErr = err
		}
		dec := yaml.NewDecoder(bytes.NewReader(front))
		dec.KnownFields(true)
		if err := dec.Decode(&a.Meta); err != nil && a.MetaErr == nil {
			a.MetaErr = err
		}
	}
	first, _, _ := strings.Cut(a.Body, "\n")
	if m := titleRe.FindStringSubmatch(first); m != nil {
		a.Title = m[2]
	}
	a.parseHeader()
	return a, nil
}

// parseHeader fills Header from the bullets between the title and the first "## " heading.
func (a *ADR) parseHeader() {
	var key string
	for i, line := range strings.Split(a.Body, "\n") {
		if i == 0 {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			return
		}
		if m := bulletR.FindStringSubmatch(line); m != nil {
			key = m[1]
			if _, seen := a.Header[key]; !seen {
				a.Header[key] = m[2]
				a.Keys = append(a.Keys, key)
			} else {
				key = ""
			}
			continue
		}
		if key != "" && strings.HasPrefix(line, "  ") {
			a.Header[key] += " " + strings.TrimSpace(line)
			continue
		}
		key = ""
	}
}

// loadADRs reads every NNNN-*.md of dir except the template, sorted by number.
func loadADRs(dir string) (adrs []*ADR, template *ADR, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !fileRe.MatchString(e.Name()) {
			continue
		}
		a, err := parseADR(dir, e.Name())
		if err != nil {
			return nil, nil, err
		}
		if a.Number == "0000" {
			template = a
			continue
		}
		adrs = append(adrs, a)
	}
	sort.Slice(adrs, func(i, j int) bool { return adrs[i].File < adrs[j].File })
	return adrs, template, nil
}

// headerNumbers returns the ADR numbers a header text names as [NNNN](file) or "ADR NNNN" outside
// parentheses: a number in a parenthetical is a note, not a link (ADR 0046).
func headerNumbers(text string) []string {
	t := linkRe.ReplaceAllString(text, "@$1@")
	t = adrRe.ReplaceAllString(t, "@$1@")
	for {
		n := parenRe.ReplaceAllString(t, "")
		if n == t {
			break
		}
		t = n
	}
	var out []string
	for _, m := range regexp.MustCompile(`@([0-9]{4})@`).FindAllStringSubmatch(t, -1) {
		out = append(out, m[1])
	}
	return uniqSorted(out)
}

// linkNumbers returns every [NNNN](file) number of the text, parentheses included.
func linkNumbers(text string) []string {
	var out []string
	for _, m := range linkRe.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return uniqSorted(out)
}

func uniqSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	s := append([]string(nil), in...)
	sort.Strings(s)
	out := s[:1]
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

func statusWord(status string) string {
	for _, p := range []struct{ prefix, word string }{
		{"Proposed", "proposed"},
		{"Accepted", "accepted"},
		{"Deprecated", "deprecated"},
		{"Superseded by", "superseded"},
		{"Rejected", "rejected"},
	} {
		if strings.HasPrefix(status, p.prefix) {
			rest := status[len(p.prefix):]
			if rest == "" || !isLetter(rest[0]) {
				return p.word
			}
		}
	}
	return ""
}

func isLetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }

// Derived relationships: who amends, supersedes or relates to an ADR.
type Derived struct {
	AmendedBy    map[string][]string
	SupersededBy map[string][]string
	RelatedBy    map[string][]string
}

func derive(adrs []*ADR) Derived {
	d := Derived{AmendedBy: map[string][]string{}, SupersededBy: map[string][]string{}, RelatedBy: map[string][]string{}}
	for _, a := range adrs {
		for _, t := range a.Meta.Amends {
			d.AmendedBy[t] = append(d.AmendedBy[t], a.Number)
		}
		for _, t := range a.Meta.Supersedes {
			d.SupersededBy[t] = append(d.SupersededBy[t], a.Number)
		}
		for _, t := range a.Meta.Relates {
			d.RelatedBy[t] = append(d.RelatedBy[t], a.Number)
		}
	}
	return d
}

func (a *ADR) String() string { return fmt.Sprintf("%s %s", a.Number, a.Title) }

// checkQuoted rejects an unquoted ADR number in amends, supersedes or relates: YAML would read 0043 as a
// number in some tools, so the files quote every number.
func checkQuoted(front []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(front, &doc); err != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil // the strict decode reports the real error
	}
	m := doc.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		switch m.Content[i].Value {
		case "amends", "supersedes", "relates":
			for _, it := range m.Content[i+1].Content {
				if it.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) == 0 {
					return fmt.Errorf("line %d: %s: ADR number %s is not quoted", it.Line, m.Content[i].Value, it.Value)
				}
			}
		}
	}
	return nil
}
