// SPDX-License-Identifier: Apache-2.0

package agentconfig_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/agentconfig"
	"github.com/procoduck/koinon/templates"
)

var _ = Describe("Harness settings", func() {
	It("copies the template's harness settings and stores them under harness", func() {
		t := &templates.Template{Spec: templates.Spec{Harness: templates.Harness{PluginInstalls: true}}}
		raw, err := agentconfig.FromTemplate(t, "sha256:x").Marshal()
		Expect(err).NotTo(HaveOccurred())
		// agentd reads the same JSON path without this package.
		Expect(string(raw)).To(ContainSubstring(`"harness":{"pluginInstalls":true}`))
		cfg, err := agentconfig.Parse(raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Harness.PluginInstalls).To(BeTrue())
	})

	It("leaves them out when every setting is off, and reads an old config as off", func() {
		raw, err := agentconfig.FromTemplate(&templates.Template{}, "sha256:x").Marshal()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).NotTo(ContainSubstring("harness"))
		cfg, err := agentconfig.Parse([]byte(`{"instructions":"x"}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Harness.PluginInstalls).To(BeFalse())
	})

	It("is on for no shipped template", func() {
		ts, err := templates.All()
		Expect(err).NotTo(HaveOccurred())
		for _, t := range ts {
			Expect(agentconfig.FromTemplate(t, t.Hash).Harness.PluginInstalls).To(BeFalse(), t.Metadata.ID)
		}
	})
})
