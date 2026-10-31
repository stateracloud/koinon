// SPDX-License-Identifier: Apache-2.0

package agentconfig_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/templates"
)

var _ = Describe("Sandbox resources", func() {
	It("copies the template's resources and stores them under resources", func() {
		res := templates.Resources{
			CPU:    templates.Quantities{Request: "250m", Limit: "2"},
			Memory: templates.Quantities{Request: "1Gi", Limit: "2Gi"},
		}
		raw, err := agentconfig.FromTemplate(&templates.Template{Spec: templates.Spec{Resources: res}}, "sha256:x").Marshal()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(ContainSubstring(`"resources":{"cpu":{"request":"250m","limit":"2"},"memory":{"request":"1Gi","limit":"2Gi"}}`))
		cfg, err := agentconfig.Parse(raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Resources).To(Equal(res))
	})

	It("leaves them out when the template sets none, and reads an old config as unset", func() {
		raw, err := agentconfig.FromTemplate(&templates.Template{}, "sha256:x").Marshal()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).NotTo(ContainSubstring("resources"))
		cfg, err := agentconfig.Parse([]byte(`{"instructions":"x"}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Resources).To(Equal(templates.Resources{}))
	})
})
