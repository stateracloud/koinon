// SPDX-License-Identifier: Apache-2.0

package templates_test

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/templates"
)

var _ = Describe("Triggers", func() {
	sched := func(cron, zone string) templates.Trigger {
		return templates.Trigger{ID: "t", Type: templates.TriggerSchedule, Filter: templates.TriggerFilter{Cron: cron, Timezone: zone}, Instruction: "go"}
	}

	It("accepts a schedule and a direct message", func() {
		Expect(templates.ValidateTriggers([]templates.Trigger{
			sched("*/15 8-17 * * mon-fri", "Europe/Tallinn"),
			{ID: "chat", Type: templates.TriggerDirectMessage, Instruction: "reply"},
		})).To(Succeed())
	})

	DescribeTable("rejects",
		func(ts []templates.Trigger, want string) {
			Expect(templates.ValidateTriggers(ts)).To(MatchError(ContainSubstring(want)))
		},
		Entry("six cron fields", []templates.Trigger{sched("0 0 9 * * *", "UTC")}, "cron"),
		Entry("a cron descriptor", []templates.Trigger{sched("@daily", "UTC")}, "five fields"),
		Entry("a timezone inside the cron", []templates.Trigger{sched("CRON_TZ=UTC 0 9 * * *", "UTC")}, "five fields"),
		Entry("an unknown zone", []templates.Trigger{sched("0 9 * * *", "Nowhere/Land")}, "IANA"),
		Entry("the Local zone", []templates.Trigger{sched("0 9 * * *", "Local")}, "IANA"),
		Entry("a duplicate id", []templates.Trigger{sched("0 9 * * *", "UTC"), sched("0 10 * * *", "UTC")}, "duplicate"),
		Entry("a bad id", []templates.Trigger{{ID: "Bad_ID", Type: templates.TriggerDirectMessage, Instruction: "x"}}, "slug"),
		Entry("two direct messages", []templates.Trigger{
			{ID: "a", Type: templates.TriggerDirectMessage, Instruction: "x"}, {ID: "b", Type: templates.TriggerDirectMessage, Instruction: "x"},
		}, "only one"),
		Entry("a reserved type", []templates.Trigger{{ID: "a", Type: templates.TriggerChannelEvent, Instruction: "x"}}, "reserved"),
		Entry("a long instruction", []templates.Trigger{{ID: "a", Type: templates.TriggerDirectMessage, Instruction: strings.Repeat("x", 4097)}}, "longer"),
	)

	It("computes the next firing in the trigger's zone", func() {
		next, ok := sched("0 9 * * *", "Europe/Tallinn").Next(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
		Expect(ok).To(BeTrue())
		Expect(next.UTC()).To(Equal(time.Date(2026, 7, 1, 6, 0, 0, 0, time.UTC))) // 09:00 EEST
	})

	It("is validated in a template, by the schema and the checks", func() {
		base := string(readFile("testdata/valid.yaml"))
		ok := base + "  triggers:\n    - id: morning\n      type: schedule\n      filter: {cron: \"0 9 * * *\", timezone: UTC}\n      instruction: Do it.\n"
		t, err := templates.Parse([]byte(ok))
		Expect(err).NotTo(HaveOccurred())
		Expect(t.Spec.Triggers).To(HaveLen(1))
		noFilter := base + "  triggers:\n    - id: morning\n      type: schedule\n      instruction: Do it.\n"
		_, err = templates.Parse([]byte(noFilter))
		Expect(err).To(HaveOccurred())
		badZone := strings.Replace(ok, "timezone: UTC", "timezone: Mars/Base", 1)
		_, err = templates.Parse([]byte(badZone))
		Expect(err).To(MatchError(ContainSubstring("IANA")))
	})
})
