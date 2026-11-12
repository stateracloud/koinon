// SPDX-License-Identifier: Apache-2.0

package templates_test

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/templates"
)

var _ = Describe("harnesses", func() {
	want := func() []string {
		var out []string
		for _, f := range templates.AllHarnesses {
			out = append(out, string(f))
		}
		return out
	}

	It("match the template schema's harness enum", func() {
		var schema struct {
			Defs struct {
				HarnessName struct {
					Enum []string `json:"enum"`
				} `json:"harness"`
			} `json:"$defs"`
		}
		Expect(json.Unmarshal(readFile("schema/template.schema.json"), &schema)).To(Succeed())
		Expect(schema.Defs.HarnessName.Enum).To(ConsistOf(want()))
	})
})
