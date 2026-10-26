// SPDX-License-Identifier: Apache-2.0

package templates

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// With one flavor the schema's enum keeps a template file from reaching these rules, so they are
// checked on built values (both return before the rules that need a full template); they matter again once a second flavor exists (ADR 0059).
var _ = Describe("check flavor rules", func() {
	base := func() *Template {
		return &Template{APIVersion: APIVersion, Kind: Kind, Spec: Spec{
			Flavors: Flavors{Allowed: []Flavor{FlavorOpenCode}, Default: FlavorOpenCode},
		}}
	}

	It("rejects a default flavor that is not allowed", func() {
		t := base()
		t.Spec.Flavors.Default = "other"
		Expect(t.check()).To(MatchError(ContainSubstring(`default flavor "other" is not in the allowed flavors`)))
	})

	It("rejects pluginInstalls when no allowed flavor supports it", func() {
		t := base()
		t.Spec.Flavors = Flavors{Allowed: []Flavor{"other"}, Default: "other"}
		t.Spec.Harness.PluginInstalls = true
		Expect(t.check()).To(MatchError(ContainSubstring("harness.pluginInstalls applies to the opencode flavor only")))
	})
})
