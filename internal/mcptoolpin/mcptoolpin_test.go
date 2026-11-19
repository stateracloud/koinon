// SPDX-License-Identifier: Apache-2.0

package mcptoolpin_test

import (
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/mcptoolpin"
)

func hash(p mcptoolpin.Published) string {
	GinkgoHelper()
	t, _, err := mcptoolpin.Normalise(p)
	Expect(err).NotTo(HaveOccurred())
	return t.Hash()
}

var _ = Describe("the tool pin", func() {
	base := mcptoolpin.Published{
		Name: "read", Title: "Read", Description: "reads an item",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}}}`),
	}

	It("gives the same hash for the same text, whatever the key order or spacing", func() {
		other := base
		other.InputSchema = json.RawMessage(`{ "properties": {"id": {"type": "string"}}, "type": "object" }`)
		Expect(hash(other)).To(Equal(hash(base)))
		Expect(hash(base)).To(MatchRegexp(mcptoolpin.HashPattern))
	})

	DescribeTable("changes the hash when the model would read other text",
		func(mutate func(*mcptoolpin.Published)) {
			p := base
			mutate(&p)
			Expect(hash(p)).NotTo(Equal(hash(base)))
		},
		Entry("description", func(p *mcptoolpin.Published) { p.Description += " and sends it away" }),
		Entry("title", func(p *mcptoolpin.Published) { p.Title = "Other" }),
		Entry("input schema", func(p *mcptoolpin.Published) {
			p.InputSchema = json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer"}}}`)
		}),
		Entry("output schema", func(p *mcptoolpin.Published) { p.OutputSchema = json.RawMessage(`{"type":"object"}`) }),
		Entry("annotation", func(p *mcptoolpin.Published) { p.Annotations = json.RawMessage(`{"readOnlyHint":true}`) }),
	)

	It("does not change the hash for a member the model never sees", func() {
		var p mcptoolpin.Published
		Expect(json.Unmarshal([]byte(`{"name":"read","title":"Read","description":"reads an item",`+
			`"inputSchema":{"type":"object","properties":{"id":{"type":"string"}}},"_meta":{"x":1},"icons":[{"src":"a"}]}`), &p)).To(Succeed())
		Expect(hash(p)).To(Equal(hash(base)))
	})

	It("uses the empty object schema when the input schema is missing", func() {
		p := mcptoolpin.Published{Name: "ping"}
		t, _, err := mcptoolpin.Normalise(p)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(t.InputSchema)).To(Equal(`{"type":"object"}`))
	})

	It("rejects a name Koinon cannot route", func() {
		for _, n := range []string{"", "a b", "a/b", strings.Repeat("a", 129)} {
			_, _, err := mcptoolpin.Normalise(mcptoolpin.Published{Name: n})
			Expect(err).To(MatchError(mcptoolpin.ErrUnusable), n)
		}
	})

	It("round-trips through JSON and Parse with the same hash", func() {
		t, _, err := mcptoolpin.Normalise(base)
		Expect(err).NotTo(HaveOccurred())
		back, err := mcptoolpin.Parse(t.JSON())
		Expect(err).NotTo(HaveOccurred())
		Expect(back.Hash()).To(Equal(t.Hash()))
	})

	It("caps a long description", func() {
		p := base
		p.Description = strings.Repeat("x", 100000)
		t, rep, err := mcptoolpin.Normalise(p)
		Expect(err).NotTo(HaveOccurred())
		Expect(rep.Capped).To(BeTrue())
		Expect(len(t.Description)).To(BeNumerically("<", 100000))
	})
})
