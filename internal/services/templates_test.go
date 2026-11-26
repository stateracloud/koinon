// SPDX-License-Identifier: Apache-2.0

package services

import (
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/templates"
)

var _ = Describe("templateFromConfig", func() {
	cfg := agentconfig.Config{
		Instructions: "do the work",
		Model:        "from-version",
		Thinking:     "on",
		Triggers: []templates.Trigger{
			{ID: "a", Type: templates.TriggerSchedule, Notify: &templates.NotifyTarget{Kind: templates.NotifyUser, User: "01a215cc-d5a8-77c9-b7c3-9cddf5de8ac1"}},
			{ID: "b", Type: templates.TriggerSchedule, Notify: &templates.NotifyTarget{Kind: templates.NotifyChannel, Channel: "c"}},
			{ID: "c", Type: templates.TriggerSchedule, Notify: &templates.NotifyTarget{Kind: templates.NotifyRole, Role: store.OrgRoleAdmin}},
		},
	}
	text := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

	It("drops a notify target that names a person or a channel and keeps a role", func() {
		t := templateFromConfig(cfg, store.Agent{Harness: "opencode"}, "x", "X", "")
		Expect(t.Spec.Triggers).To(HaveLen(3))
		Expect(t.Spec.Triggers[0].Notify).To(BeNil())
		Expect(t.Spec.Triggers[1].Notify).To(BeNil())
		Expect(t.Spec.Triggers[2].Notify).NotTo(BeNil())
		// The agent's version is not changed.
		Expect(cfg.Triggers[0].Notify).NotTo(BeNil())
	})

	It("takes the agent's own model, thinking and sandbox resources over the version's", func() {
		a := store.Agent{
			Harness: "opencode", Model: text("agent-model"), Thinking: text("off"),
			SandboxCpuLimit: text("2"), SandboxMemoryRequest: text("1Gi"),
		}
		t := templateFromConfig(cfg, a, "x", "X", "")
		Expect(t.Spec.Model.Name).To(Equal("agent-model"))
		Expect(t.Spec.Model.Thinking).To(Equal("off"))
		Expect(t.Spec.Resources.CPU.Limit).To(Equal("2"))
		Expect(t.Spec.Resources.Memory.Request).To(Equal("1Gi"))
	})

	It("falls back to the version's values when the agent sets none", func() {
		t := templateFromConfig(cfg, store.Agent{Harness: "opencode"}, "x", "X", "")
		Expect(t.Spec.Model.Name).To(Equal("from-version"))
		Expect(t.Spec.Model.Thinking).To(Equal("on"))
	})
})
