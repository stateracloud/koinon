// SPDX-License-Identifier: Apache-2.0

package templates

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// With one harness the schema's enum keeps a template file from reaching these rules, so they are
// checked on built values (both return before the rules that need a full template); they matter again once a second harness exists (ADR 0059).
var _ = Describe("check harness rules", func() {
	base := func() *Template {
		return &Template{APIVersion: APIVersion, Kind: Kind, Spec: Spec{
			Harness: Harness{Allowed: []HarnessName{HarnessOpenCode}, Default: HarnessOpenCode},
		}}
	}

	It("rejects a default harness that is not allowed", func() {
		t := base()
		t.Spec.Harness.Default = "other"
		Expect(t.check()).To(MatchError(ContainSubstring(`default harness "other" is not in the allowed harnesses`)))
	})

	It("rejects pluginInstalls when no allowed harness supports it", func() {
		t := base()
		t.Spec.Harness = Harness{Allowed: []HarnessName{"other"}, Default: "other"}
		t.Spec.Harness.PluginInstalls = true
		Expect(t.check()).To(MatchError(ContainSubstring("harness.pluginInstalls applies to the opencode harness only")))
	})
})
