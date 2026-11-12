// SPDX-License-Identifier: Apache-2.0

package agentconfig_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/templates"
)

var _ = Describe("Harness settings", func() {
	It("copies the template's harness settings and stores them under harness", func() {
		t := &templates.Template{Spec: templates.Spec{Harness: templates.Harness{HarnessSettings: templates.HarnessSettings{PluginInstalls: true}}}}
		raw, err := agentconfig.FromTemplate(t, "sha256:x").Marshal()
		Expect(err).NotTo(HaveOccurred())
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

var _ = Describe("The thinking setting", func() {
	It("copies the template's starting value and leaves it out when the template names none", func() {
		raw, err := agentconfig.FromTemplate(&templates.Template{Spec: templates.Spec{Model: templates.Model{Name: "default", Thinking: "off"}}}, "sha256:x").Marshal()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(ContainSubstring(`"thinking":"off"`))
		cfg, err := agentconfig.Parse(raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Thinking).To(Equal("off"))

		raw, err = agentconfig.FromTemplate(&templates.Template{}, "sha256:x").Marshal()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).NotTo(ContainSubstring("thinking"))
	})
})

var _ = Describe("WebEgress", func() {
	// No grant opens arbitrary web hosts yet (ADR 0080); a grant type that does adds its case here.
	It("is false for a version with the usual grants", func() {
		c := agentconfig.Config{
			Grants:  []templates.Grant{{Connector: "github", Scopes: []string{"repo.read"}, Preset: templates.PresetStandard}},
			Harness: templates.HarnessSettings{PluginInstalls: true},
		}
		Expect(c.WebEgress()).To(BeFalse())
		Expect(agentconfig.Config{}.WebEgress()).To(BeFalse())
	})
})

var _ = Describe("IsEphemeral", func() {
	It("is off by default, including for a config stored before it existed", func() {
		cfg, err := agentconfig.Parse([]byte(`{"instructions":"x"}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(agentconfig.IsEphemeral(cfg)).To(BeFalse())
		raw, err := agentconfig.FromTemplate(&templates.Template{}, "sha256:x").Marshal()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).NotTo(ContainSubstring("ephemeral"))
	})

	It("follows the template and survives storage", func() {
		t := &templates.Template{Spec: templates.Spec{Ephemeral: true}}
		raw, err := agentconfig.FromTemplate(t, "sha256:x").Marshal()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(ContainSubstring(`"ephemeral":true`))
		cfg, err := agentconfig.Parse(raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(agentconfig.IsEphemeral(cfg)).To(BeTrue())
	})

	It("is on for no shipped template", func() {
		ts, err := templates.All()
		Expect(err).NotTo(HaveOccurred())
		for _, t := range ts {
			Expect(agentconfig.IsEphemeral(agentconfig.FromTemplate(t, t.Hash))).To(BeFalse(), t.Metadata.ID)
		}
	})
})

var _ = Describe("Parse", func() {
	It("reads back what FromTemplate writes, with the model as a plain string", func() {
		t := &templates.Template{Spec: templates.Spec{Instructions: "help", Model: templates.Model{Name: "fake/model-1"}}}
		raw, err := agentconfig.FromTemplate(t, "sha256:x").Marshal()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(ContainSubstring(`"model":"fake/model-1"`))
		cfg, err := agentconfig.Parse(raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Model).To(Equal("fake/model-1"))
		Expect(cfg.Instructions).To(Equal("help"))
	})

	It("reads an empty config as the zero config", func() {
		Expect(agentconfig.Parse(nil)).To(BeZero())
	})

	It("rejects a model that is not a string", func() {
		_, err := agentconfig.Parse([]byte(`{"model":{"name":"fake/model-1"}}`))
		Expect(err).To(MatchError(ContainSubstring("decode agent config")))
	})
})

var _ = Describe("QuestionTimers", func() {
	It("defaults to a reminder after 24 hours and expiry after 7 days", func() {
		r, e := agentconfig.Config{}.QuestionTimers()
		Expect(r).To(Equal(24 * time.Hour))
		Expect(e).To(Equal(7 * 24 * time.Hour))
	})

	It("follows the template, survives storage, and fills in what is unset", func() {
		t := &templates.Template{Spec: templates.Spec{Questions: templates.QuestionTimers{
			ReminderAfter: templates.Duration{Duration: 2 * time.Hour},
		}}}
		raw, err := agentconfig.FromTemplate(t, "sha256:x").Marshal()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(ContainSubstring(`"questions":{"reminderAfter":"2h0m0s"}`))
		cfg, err := agentconfig.Parse(raw)
		Expect(err).NotTo(HaveOccurred())
		r, e := cfg.QuestionTimers()
		Expect(r).To(Equal(2 * time.Hour))
		Expect(e).To(Equal(7 * 24 * time.Hour))
	})

	It("drops a reminder that would not come before the expiry", func() {
		cfg := agentconfig.Config{Questions: templates.QuestionTimers{ExpireAfter: templates.Duration{Duration: 6 * time.Hour}}}
		r, e := cfg.QuestionTimers()
		Expect(r).To(BeZero())
		Expect(e).To(Equal(6 * time.Hour))
	})
})

var _ = Describe("RunLimits", func() {
	It("gives a template that sets only the deprecated maxTokens the platform defaults", func() {
		t := &templates.Template{Spec: templates.Spec{Limits: templates.Limits{
			Run: templates.RunLimits{MaxDuration: templates.Duration{Duration: 10 * time.Minute}, MaxTokens: 2_000_000}, //nolint:staticcheck // the deprecated field is the case under test
		}}}
		raw, err := agentconfig.FromTemplate(t, "sha256:x").Marshal()
		Expect(err).NotTo(HaveOccurred())
		cfg, err := agentconfig.Parse(raw)
		Expect(err).NotTo(HaveOccurred())
		l := cfg.RunLimits()
		Expect(l.WallClock).To(Equal(10 * time.Minute))
		Expect(l.MaxModelRequests).To(Equal(int64(200)))
		Expect(l.MaxSpend.String()).To(Equal(agentconfig.DefaultMaxSpend.String()))
		Expect(l.MaxSpend.String()).To(Equal("5.00"))
	})

	It("keeps a template's explicit values", func() {
		t := &templates.Template{Spec: templates.Spec{Limits: templates.Limits{
			Run: templates.RunLimits{MaxModelRequests: 100, MaxSpendUSD: templates.USD(75)},
		}}}
		l := agentconfig.FromTemplate(t, "sha256:x").RunLimits()
		Expect(l.MaxModelRequests).To(Equal(int64(100)))
		Expect(l.MaxSpend.String()).To(Equal("0.75"))
	})
})
