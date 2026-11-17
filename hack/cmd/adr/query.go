// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"slices"
	"strings"
)

func runFind(adrs []*ADR, words []string, area, tag, status string, text bool, stdout io.Writer) (int, error) {
	n := 0
	for _, a := range adrs {
		if area != "" && a.Meta.Area != area || tag != "" && !slices.Contains(a.Meta.Tags, tag) || status != "" && a.Meta.Status != status {
			continue
		}
		hay := strings.ToLower(a.Number + " " + a.Title + " " + strings.Join(a.Meta.Tags, " ") + " " + a.Meta.Area)
		if text {
			hay += " " + strings.ToLower(a.Body)
		}
		ok := true
		for _, w := range words {
			ok = ok && strings.Contains(hay, strings.ToLower(w))
		}
		if !ok {
			continue
		}
		n++
		pf(stdout, "%s  %-10s %-20s %s\n", a.Number, a.Meta.Status, a.Meta.Area, a.Title)
	}
	if n == 0 {
		return 1, nil
	}
	return 0, nil
}

func byNumber(adrs []*ADR, n string) *ADR {
	for _, a := range adrs {
		if a.Number == n {
			return a
		}
	}
	return nil
}

func runShow(adrs []*ADR, args []string, body bool, stdout, stderr io.Writer) (int, error) {
	if len(args) != 1 {
		pln(stderr, "usage: adr show [--body] NNNN")
		return 2, nil
	}
	a := byNumber(adrs, pad(args[0]))
	if a == nil {
		pf(stderr, "adr: no ADR %s\n", args[0])
		return 1, nil
	}
	d := derive(adrs)
	pf(stdout, "%s. %s\n  file:    docs/adr/%s\n  status:  %s\n  date:    %s\n  area:    %s\n  tags:    %s\n",
		a.Number, a.Title, a.File, a.Meta.Status, a.Meta.Date, a.Meta.Area, strings.Join(a.Meta.Tags, ", "))
	if a.Meta.Decides != "" {
		pf(stdout, "  decides: %s\n", a.Meta.Decides)
	}
	for _, l := range []struct {
		name string
		nums []string
	}{
		{"amends", a.Meta.Amends},
		{"amended by", d.AmendedBy[a.Number]},
		{"supersedes", a.Meta.Supersedes},
		{"superseded by", d.SupersededBy[a.Number]},
		{"relates to", a.Meta.Relates},
		{"related by", d.RelatedBy[a.Number]},
	} {
		for _, n := range l.nums {
			t := ""
			if o := byNumber(adrs, n); o != nil {
				t = o.Title
			}
			pf(stdout, "  %-14s %s %s\n", l.name+":", n, t)
		}
	}
	if body {
		pf(stdout, "\n%s", a.Body)
	}
	return 0, nil
}

func runGraph(adrs []*ADR, args []string, relates bool, depth int, stdout, stderr io.Writer) (int, error) {
	o := graphOpts{relates: relates, depth: depth}
	if len(args) > 1 {
		pln(stderr, "usage: adr graph [--relates] [--depth N] [NNNN]")
		return 2, nil
	}
	if len(args) == 1 {
		o.focus = pad(args[0])
		if byNumber(adrs, o.focus) == nil {
			pf(stderr, "adr: no ADR %s\n", args[0])
			return 1, nil
		}
	}
	pp(stdout, mermaid(adrs, o))
	return 0, nil
}

// pad turns "46" or "0046" into "0046".
func pad(n string) string {
	n = strings.TrimLeft(n, "0")
	return strings.Repeat("0", max(0, 4-len(n))) + n
}
