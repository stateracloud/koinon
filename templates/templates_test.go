// SPDX-License-Identifier: Apache-2.0

package templates_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing/fstest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/templates"
)

func readFile(name string) []byte {
	GinkgoHelper()
	data, err := os.ReadFile(name)
	Expect(err).NotTo(HaveOccurred())
	return data
}

// invalidFixtures returns one Entry per testdata/invalid file, with the error
// substring from its "# want:" line.
func invalidFixtures() []TableEntry {
	names, err := filepath.Glob("testdata/invalid/*.yaml")
	if err != nil || len(names) == 0 {
		panic("no invalid fixtures found")
	}
	entries := make([]TableEntry, 0, len(names))
	for _, name := range names {
		data, err := os.ReadFile(name)
		if err != nil {
			panic(err)
		}
		var want string
		for line := range strings.Lines(string(data)) {
			if w, ok := strings.CutPrefix(line, "# want: "); ok {
				want = strings.TrimSpace(w)
				break
			}
		}
		if want == "" {
			panic(name + ` has no "# want:" line`)
		}
		entries = append(entries, Entry(filepath.Base(name), data, want))
	}
	return entries
}

var _ = Describe("Schema", func() {
	It("declares the apiVersion derived from internal/project", func() {
		var schema struct {
			Properties struct {
				APIVersion struct {
					Const string `json:"const"`
				} `json:"apiVersion"`
			} `json:"properties"`
		}
		Expect(json.Unmarshal(templates.Schema(), &schema)).To(Succeed())
		Expect(schema.Properties.APIVersion.Const).To(Equal(templates.APIVersion))
	})
})

var _ = Describe("Embedded templates", func() {
	It("all validate", func() {
		ts, err := templates.All()
		Expect(err).NotTo(HaveOccurred())
		Expect(ts).NotTo(BeEmpty())
		for _, t := range ts {
			Expect(t.Hash).To(MatchRegexp(`^sha256:[0-9a-f]{64}$`))
		}
	})

	It("embeds every *.yaml file in templates/", func() {
		files, err := filepath.Glob("*.yaml")
		Expect(err).NotTo(HaveOccurred())
		ts, err := templates.All()
		Expect(err).NotTo(HaveOccurred())
		ids := make([]string, 0, len(ts))
		for _, t := range ts {
			ids = append(ids, t.Metadata.ID+".yaml")
		}
		Expect(ids).To(ConsistOf(files))
	})

	Describe("repo-helper", func() {
		var t *templates.Template

		BeforeEach(func() {
			var ok bool
			t, ok = templates.Get("repo-helper")
			Expect(ok).To(BeTrue())
		})

		It("offers the three v0.1 flavors with OpenCode as default", func() {
			Expect(t.Spec.Flavors.Allowed).To(ConsistOf(
				templates.FlavorOpenCode, templates.FlavorHermes, templates.FlavorPi))
			Expect(t.Spec.Flavors.Default).To(Equal(templates.FlavorOpenCode))
		})

		It("suggests the GitHub v0.1 scopes on the Standard preset for one repository", func() {
			Expect(t.Spec.Grants).To(HaveLen(1))
			g := t.Spec.Grants[0]
			Expect(g.Connector).To(Equal("github"))
			Expect(g.Scopes).To(ConsistOf("repo.read", "pull_request.create", "issue.comment"))
			Expect(g.Preset).To(Equal(templates.PresetStandard))
			Expect(g.Targets).To(Equal(&templates.Targets{Min: 1, Max: 1}))
		})

		It("caps runs at 30 minutes and a token budget, with a spend limit", func() {
			Expect(t.Spec.Limits.Run.MaxDuration.Duration).To(Equal(30 * time.Minute))
			Expect(t.Spec.Limits.Run.MaxTokens).To(BeNumerically(">", 0))
			Expect(t.Spec.Limits.SpendUSD.Cents()).To(BeNumerically(">", 0))
		})

		It("tells the agent the rules the tools enforce", func() {
			Expect(t.Spec.Instructions).To(ContainSubstring("create_pull_request"))
			Expect(t.Spec.Instructions).To(ContainSubstring(".github/workflows/"))
		})
	})

	It("returns false for an unknown ID", func() {
		_, ok := templates.Get("no-such-template")
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("Parse", func() {
	var valid []byte

	BeforeEach(func() {
		valid = readFile("testdata/valid.yaml")
	})

	It("accepts the minimal valid fixture", func() {
		t, err := templates.Parse(valid)
		Expect(err).NotTo(HaveOccurred())
		Expect(t.Metadata.ID).To(Equal("fixture"))
		Expect(t.Spec.Limits.SpendUSD.String()).To(Equal("10.00"))
		Expect(t.Spec.Limits.SpendUSD.Cents()).To(Equal(int64(1000)))
	})

	DescribeTable("rejects invalid fixtures",
		func(data []byte, want string) {
			_, err := templates.Parse(data)
			Expect(err).To(MatchError(ContainSubstring(want)))
		},
		invalidFixtures(),
	)

	Describe("hash", func() {
		It("is stable across parses", func() {
			a, err := templates.Parse(valid)
			Expect(err).NotTo(HaveOccurred())
			b, err := templates.Parse(valid)
			Expect(err).NotTo(HaveOccurred())
			Expect(a.Hash).To(Equal(b.Hash))
		})

		It("ignores comments, key order and YAML style", func() {
			a, err := templates.Parse(valid)
			Expect(err).NotTo(HaveOccurred())
			restyled := strings.Replace(string(valid),
				"  model:\n    name: default\n", "  # a comment\n  model: {name: \"default\"}\n", 1)
			restyled = strings.Replace(restyled,
				"kind: Template\n", "", 1) + "kind: Template\n"
			Expect(restyled).NotTo(Equal(string(valid)))
			b, err := templates.Parse([]byte(restyled))
			Expect(err).NotTo(HaveOccurred())
			Expect(b.Hash).To(Equal(a.Hash))
		})

		It("changes when the content changes", func() {
			a, err := templates.Parse(valid)
			Expect(err).NotTo(HaveOccurred())
			b, err := templates.Parse([]byte(strings.Replace(string(valid), "Do the thing.", "Do another thing.", 1)))
			Expect(err).NotTo(HaveOccurred())
			Expect(b.Hash).NotTo(Equal(a.Hash))
		})
	})
})

var _ = Describe("Load", func() {
	It("requires the file name to match the template ID", func() {
		fsys := fstest.MapFS{"other.yaml": {Data: readFile("testdata/valid.yaml")}}
		_, err := templates.Load(fsys)
		Expect(err).To(MatchError(ContainSubstring("file name must be fixture.yaml")))
	})

	It("names the file in validation errors", func() {
		fsys := fstest.MapFS{"fixture.yaml": {Data: readFile("testdata/invalid/unknown-preset.yaml")}}
		_, err := templates.Load(fsys)
		Expect(err).To(MatchError(HavePrefix("fixture.yaml: ")))
	})

	It("loads valid files sorted by ID", func() {
		valid := readFile("testdata/valid.yaml")
		other := []byte(strings.Replace(string(valid), "id: fixture", "id: another", 1))
		fsys := fstest.MapFS{
			"fixture.yaml": {Data: valid},
			"another.yaml": {Data: other},
			"README.md":    {Data: []byte("ignored")},
		}
		ts, err := templates.Load(fsys)
		Expect(err).NotTo(HaveOccurred())
		Expect(ts).To(HaveLen(2))
		Expect(ts[0].Metadata.ID).To(Equal("another"))
		Expect(ts[1].Metadata.ID).To(Equal("fixture"))
	})
})
