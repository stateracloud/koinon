// SPDX-License-Identifier: Apache-2.0

package templates_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing/fstest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/templates"
)

// platformTools are the tools Koinon itself serves to every agent.
var platformTools = []string{gateway.ToolAskUser}

// testCatalog is the catalog the shipped templates are held to: the embedded connector types, their
// scope tools (also under the connector's name, as the MCP server shows them), the platform tools
// and the tools with a declaration in testdata/legs/tool-declarations.json.
func testCatalog() templates.Catalog {
	all, err := connectors.All()
	Expect(err).NotTo(HaveOccurred())
	cat := templates.Catalog{Scopes: map[string][]string{}}
	known := map[string]bool{}
	for _, n := range platformTools {
		known[n] = true
	}
	for name := range declarations() {
		known[name] = true
	}
	for _, c := range all {
		for _, s := range c.Spec.Scopes {
			cat.Scopes[c.Metadata.ID] = append(cat.Scopes[c.Metadata.ID], s.ID)
			for _, tool := range s.Tools {
				known[tool] = true
				known[c.Metadata.ID+"_"+tool] = true
			}
		}
	}
	cat.KnownTool = func(n string) bool { return known[n] }
	return cat
}

func wantLine(data []byte) string {
	for line := range strings.Lines(string(data)) {
		if w, ok := strings.CutPrefix(line, "# want: "); ok {
			return strings.TrimSpace(w)
		}
	}
	return ""
}

var _ = Describe("Rules", func() {
	It("has unique IDs", func() {
		seen := map[string]bool{}
		for _, r := range templates.Rules {
			Expect(seen[r.ID]).To(BeFalse(), r.ID)
			seen[r.ID] = true
			Expect(r.Rule).NotTo(BeEmpty(), r.ID)
		}
	})

	It("has a failing fixture per rule and a rule per fixture", func() {
		byDir := map[string][]string{}
		for _, r := range templates.Rules {
			if dir, ok := r.Stage.FixtureDir(); ok {
				byDir[dir] = append(byDir[dir], r.ID)
			}
		}
		for _, dir := range []string{"invalid", "load", "catalog", "shipped"} {
			files, err := filepath.Glob("testdata/" + dir + "/*.yaml")
			Expect(err).NotTo(HaveOccurred())
			var stems []string
			for _, f := range files {
				stems = append(stems, strings.TrimSuffix(filepath.Base(f), ".yaml"))
			}
			Expect(stems).To(ConsistOf(byDir[dir]), "testdata/"+dir)
		}
	})

	It("lists every rule in the README with its stage and fixture", func() {
		readme, err := os.ReadFile("README.md")
		Expect(err).NotTo(HaveOccurred())
		stageText := map[templates.Stage]string{
			templates.StageParse:   "`Parse` (YAML)",
			templates.StageSchema:  "`schema/template.schema.json`",
			templates.StageCheck:   "`Template.check`",
			templates.StageBuilt:   "`Template.check`",
			templates.StageLoad:    "`Load`",
			templates.StageCatalog: "`Template.CheckCatalog`",
			templates.StageShipped: "`Template.CheckShipped`",
		}
		type row struct{ stage, fixture string }
		got := map[string]row{}
		for _, line := range strings.Split(string(readme), "\n") {
			cells := strings.Split(line, "|")
			if len(cells) != 6 || !strings.HasPrefix(cells[1], " `") {
				continue
			}
			id := strings.Trim(strings.TrimSpace(cells[1]), "`")
			Expect(got).NotTo(HaveKey(id), "duplicate README row")
			got[id] = row{strings.TrimSpace(cells[2]), strings.TrimSpace(cells[4])}
		}
		want := map[string]row{}
		for _, r := range templates.Rules {
			fixture := "built value in `check_internal_test.go`"
			if dir, ok := r.Stage.FixtureDir(); ok {
				fixture = "`testdata/" + dir + "/" + r.ID + ".yaml`"
			}
			want[r.ID] = row{stageText[r.Stage], fixture}
		}
		Expect(got).To(Equal(want))
	})

	It("proves the built-value rules in check_internal_test.go", func() {
		src, err := os.ReadFile("check_internal_test.go")
		Expect(err).NotTo(HaveOccurred())
		for _, r := range templates.Rules {
			if r.Stage == templates.StageBuilt {
				Expect(string(src)).To(ContainSubstring("rule "+r.ID), r.ID)
			}
		}
	})

	It("passes the valid fixture at every stage", func() {
		t, err := templates.Parse(readFile("testdata/valid.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(t.CheckCatalog(testCatalog())).To(Succeed())
		Expect(t.CheckShipped()).To(Succeed())
	})

	It("holds the embedded templates to every stage", func() {
		Expect(templates.CheckEmbedded(testCatalog())).To(Succeed())
	})

	for _, r := range templates.Rules {
		dir, ok := r.Stage.FixtureDir()
		if !ok {
			continue
		}
		It("refuses "+r.ID+" at the "+string(r.Stage)+" stage: "+r.Rule, func() {
			data := readFile("testdata/" + dir + "/" + r.ID + ".yaml")
			want := wantLine(data)
			Expect(want).NotTo(BeEmpty(), "the fixture has a want line")
			var err error
			switch r.Stage {
			case templates.StageParse, templates.StageSchema, templates.StageCheck:
				_, err = templates.Parse(data)
			case templates.StageLoad:
				_, err = templates.Load(fstest.MapFS{r.ID + ".yaml": {Data: data}})
			case templates.StageCatalog:
				var t *templates.Template
				t, err = templates.Parse(data)
				Expect(err).NotTo(HaveOccurred(), "the fixture is valid up to the stage")
				err = t.CheckCatalog(testCatalog())
			case templates.StageShipped:
				var t *templates.Template
				t, err = templates.Parse(data)
				Expect(err).NotTo(HaveOccurred(), "the fixture is valid up to the stage")
				err = t.CheckShipped()
			}
			Expect(err).To(MatchError(ContainSubstring(want)))
			// The stage in the table is where the rule fires: the schema's errors start with
			// "schema:", the later rules of Parse do not.
			if r.Stage == templates.StageSchema {
				Expect(err).To(MatchError(HavePrefix("schema: ")))
			}
			if r.Stage == templates.StageCheck {
				Expect(err).NotTo(MatchError(HavePrefix("schema: ")))
			}
		})
	}
})
