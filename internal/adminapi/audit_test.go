// SPDX-License-Identifier: Apache-2.0

package adminapi_test

import (
	"net/http"
	"net/url"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

type auditPage struct {
	Events []struct {
		ID        string         `json:"id"`
		ActorKind string         `json:"actorKind"`
		ActorID   string         `json:"actorId"`
		Action    string         `json:"action"`
		Summary   map[string]any `json:"summary"`
		ChainSeq  int64          `json:"chainSeq"`
		PrevHash  string         `json:"prevHash"`
		Hash      string         `json:"hash"`
	} `json:"events"`
	NextPageToken string `json:"nextPageToken"`
}

var _ = Describe("GET /admin/v1/audit", func() {
	It("reads the token's org only, filters, and pages", func(ctx SpecContext) {
		e := newEnv(ctx)
		other, otherUser, _ := pgtest.NewOrg(ctx, pool)
		q := store.New(pool)
		for _, action := range []string{"agent.create", "agent.archive", "approval.approve", "approval.deny", "provider.create"} {
			Expect(audit.RecordErr(ctx, q, audit.Event{Org: e.org, Actor: audit.User(e.user), Action: action, TargetKind: "test"})).To(Succeed())
		}
		Expect(audit.RecordErr(ctx, q, audit.Event{Org: other, Actor: audit.User(otherUser), Action: "agent.create", TargetKind: "other-org-secret"})).To(Succeed())

		st, raw := e.call(ctx, e.token, http.MethodGet, "/audit?actionPrefix=agent.", nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))
		page := decode[auditPage](raw)
		Expect(page.Events).To(HaveLen(2))
		Expect(page.Events[0].Action).To(Equal("agent.archive"), "newest first")
		Expect(page.Events[0].ActorID).To(Equal(e.user.String()))
		Expect(page.Events[0].ChainSeq).To(BeNumerically(">", 0))
		Expect(page.Events[0].Hash).To(HaveLen(64))
		Expect(string(raw)).NotTo(ContainSubstring("other-org-secret"), "another org's events never show")

		By("the token's own calls are in the log too, and a filter by actor kind keeps only that kind")
		st, raw = e.call(ctx, e.token, http.MethodGet, "/audit?actorKind=token", nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))
		for _, ev := range decode[auditPage](raw).Events {
			Expect(ev.ActorKind).To(Equal("token"))
		}

		By("pages")
		st, raw = e.call(ctx, e.token, http.MethodGet, "/audit?actorKind=user&actorId="+e.user.String()+"&pageSize=2", nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))
		first := decode[auditPage](raw)
		Expect(first.Events).To(HaveLen(2))
		Expect(first.NextPageToken).NotTo(BeEmpty())
		seen := map[string]bool{first.Events[0].ID: true, first.Events[1].ID: true}
		token := first.NextPageToken
		total := 2
		for token != "" {
			st, raw = e.call(ctx, e.token, http.MethodGet, "/audit?actorKind=user&actorId="+e.user.String()+"&pageSize=2&pageToken="+url.QueryEscape(token), nil)
			Expect(st).To(Equal(http.StatusOK), string(raw))
			p := decode[auditPage](raw)
			for _, ev := range p.Events {
				Expect(seen).NotTo(HaveKey(ev.ID))
				seen[ev.ID] = true
				total++
			}
			token = p.NextPageToken
		}
		Expect(total).To(BeNumerically(">=", 5))

		By("a time range outside the log is empty")
		since := url.QueryEscape(time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		st, raw = e.call(ctx, e.token, http.MethodGet, "/audit?since="+since, nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))
		Expect(decode[auditPage](raw).Events).To(BeEmpty())
	})

	It("refuses a bad filter and a call without a token", func(ctx SpecContext) {
		e := newEnv(ctx)
		st, _ := e.call(ctx, e.token, http.MethodGet, "/audit?actionPrefix=A%25", nil)
		Expect(st).To(Equal(http.StatusBadRequest))
		st, _ = e.call(ctx, e.token, http.MethodGet, "/audit?actorId=x", nil)
		Expect(st).To(Equal(http.StatusBadRequest))
		st, _ = e.call(ctx, e.token, http.MethodGet, "/audit?pageToken=%23%23", nil)
		Expect(st).To(Equal(http.StatusBadRequest))
		st, _ = e.call(ctx, "", http.MethodGet, "/audit", nil)
		Expect(st).To(Equal(http.StatusUnauthorized))
	})

	It("shows a token of another org its own org only", func(ctx SpecContext) {
		e := newEnv(ctx)
		o2 := newEnv(ctx)
		st, raw := o2.call(ctx, o2.token, http.MethodGet, "/audit", nil)
		Expect(st).To(Equal(http.StatusOK))
		for _, ev := range decode[auditPage](raw).Events {
			Expect(ev.ActorID).NotTo(Equal(e.user.String()))
		}
	})
})
