// SPDX-License-Identifier: Apache-2.0

package adminapi_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/adminapi"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/tokens"
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
		for _, action := range []string{"agent.create", "agent.archive", "approval.approve", "approval.deny", "provider.create"} {
			_, err := audit.InTx(ctx, pool, func(_ *store.Queries, b audit.Batch) error {
				b.Org(audit.Event{Org: e.org, Actor: audit.User(e.user), Action: action, TargetKind: "test"})
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
		_, err := audit.InTx(ctx, pool, func(_ *store.Queries, b audit.Batch) error {
			b.Org(audit.Event{Org: other, Actor: audit.User(otherUser), Action: "agent.create", TargetKind: "other-org-secret"})
			return nil
		})
		Expect(err).NotTo(HaveOccurred())

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

var _ = Describe("GET /admin/v1/audit without an org", func() {
	It("answers 403 for a token with no org", func(ctx SpecContext) {
		e := newEnv(ctx)
		// A token row cannot exist without an org (org_id is part of its key), so this is the
		// handler's own guard; the unit under test is audit.List's refusal of a zero org.
		_, _, err := audit.List(ctx, store.New(pool), audit.Filter{})
		Expect(err).To(MatchError(audit.ErrBadFilter))
		st, _ := e.call(ctx, e.token, http.MethodGet, "/audit", nil)
		Expect(st).To(Equal(http.StatusOK))
	})
})

var _ = Describe("GET /admin/v1/audit?scope=install", func() {
	It("serves the install chain to a token rooted at a platform admin, checked on every request", func(ctx SpecContext) {
		e := newEnv(ctx)
		_, err := audit.InTx(ctx, pool, func(_ *store.Queries, b audit.Batch) error {
			b.Install(audit.InstallEvent{
				Actor: audit.System("koinon-test:spec"), Action: "platform_price.set", TargetKind: "platform_price", TargetID: "adminapi-install-scope",
			})
			return nil
		})
		Expect(err).NotTo(HaveOccurred())

		By("an org-admin token whose root user is no platform admin gets 403, owner or admin role")
		st, raw := e.call(ctx, e.token, http.MethodGet, "/audit?scope=install", nil)
		Expect(st).To(Equal(http.StatusForbidden), string(raw))
		adminTok, err := e.tokens.Create(ctx, e.org, audit.User(e.user), tokens.Creator{User: e.user, Role: store.OrgRoleOwner}, "admin", store.OrgRoleAdmin, 0)
		Expect(err).NotTo(HaveOccurred())
		st, raw = e.call(ctx, adminTok.Secret, http.MethodGet, "/audit?scope=install", nil)
		Expect(st).To(Equal(http.StatusForbidden), string(raw))

		By("the org scope is unchanged, and scope=org names it")
		st, _ = e.call(ctx, e.token, http.MethodGet, "/audit?scope=org", nil)
		Expect(st).To(Equal(http.StatusOK))

		By("once the root user is a platform admin the same token reads the install chain")
		_, err = pool.Exec(ctx, `INSERT INTO platform_admins (user_id) VALUES ($1)`, e.user)
		Expect(err).NotTo(HaveOccurred())
		st, raw = e.call(ctx, e.token, http.MethodGet, "/audit?scope=install&actionPrefix=platform_price.", nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))
		page := decode[auditPage](raw)
		Expect(page.Events).NotTo(BeEmpty())
		Expect(page.Events[0].ActorID).To(Equal("koinon-test:spec"))
		Expect(page.Events[0].ChainSeq).To(BeNumerically(">", 0))
		Expect(string(raw)).NotTo(ContainSubstring("orgId"))

		By("and loses it the moment the admin is removed")
		_, err = pool.Exec(ctx, `DELETE FROM platform_admins WHERE user_id = $1`, e.user)
		Expect(err).NotTo(HaveOccurred())
		st, _ = e.call(ctx, e.token, http.MethodGet, "/audit?scope=install", nil)
		Expect(st).To(Equal(http.StatusForbidden))

		By("an unknown scope is a bad request")
		st, _ = e.call(ctx, e.token, http.MethodGet, "/audit?scope=all", nil)
		Expect(st).To(Equal(http.StatusBadRequest))
	})
})

var _ = Describe("GET /admin/v1/audit/export", func() {
	It("exports the token's org as a file that verifies off the cluster, and nothing of another org", func(ctx SpecContext) {
		e := newEnv(ctx)
		other, otherUser, _ := pgtest.NewOrg(ctx, pool)
		for _, action := range []string{"agent.create", "agent.archive", "approval.approve"} {
			_, err := audit.InTx(ctx, pool, func(_ *store.Queries, b audit.Batch) error {
				b.Org(audit.Event{Org: e.org, Actor: audit.User(e.user), Action: action, TargetKind: "test"})
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
		_, err := audit.InTx(ctx, pool, func(_ *store.Queries, b audit.Batch) error {
			b.Org(audit.Event{Org: other, Actor: audit.User(otherUser), Action: "agent.create", TargetKind: "other-org-secret"})
			return nil
		})
		Expect(err).NotTo(HaveOccurred())

		st, raw := e.call(ctx, e.token, http.MethodGet, "/audit/export", nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))
		Expect(string(raw)).NotTo(ContainSubstring("other-org-secret"))
		res, err := audit.VerifyExport(bytes.NewReader(raw), audit.ExportVerifyOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Break).To(BeNil())
		Expect(res.Header.Chain).To(Equal("org"))
		Expect(res.Header.OrgID).To(Equal(e.org.String()))
		Expect(res.Rows).To(BeNumerically(">=", 3))

		By("scope=org and the token's own org name the same thing")
		st, _ = e.call(ctx, e.token, http.MethodGet, "/audit/export?scope=org&org="+e.org.String(), nil)
		Expect(st).To(Equal(http.StatusOK))

		By("a partial export starts at fromSeq")
		st, raw = e.call(ctx, e.token, http.MethodGet, "/audit/export?fromSeq=2", nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))
		res, err = audit.VerifyExport(bytes.NewReader(raw), audit.ExportVerifyOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Break).To(BeNil())
		Expect(res.Header.FromSeq).To(BeEquivalentTo(2))
	})

	It("refuses another org, the install scope for an org admin, a bad scope and no token", func(ctx SpecContext) {
		e := newEnv(ctx)
		other, _, _ := pgtest.NewOrg(ctx, pool)

		st, raw := e.call(ctx, e.token, http.MethodGet, "/audit/export?scope=org&org="+other.String(), nil)
		Expect(st).To(Equal(http.StatusForbidden), string(raw))
		st, raw = e.call(ctx, e.token, http.MethodGet, "/audit/export?org="+other.String(), nil)
		Expect(st).To(Equal(http.StatusForbidden), string(raw))

		st, raw = e.call(ctx, e.token, http.MethodGet, "/audit/export?scope=install", nil)
		Expect(st).To(Equal(http.StatusForbidden), string(raw))
		adminTok, err := e.tokens.Create(ctx, e.org, audit.User(e.user), tokens.Creator{User: e.user, Role: store.OrgRoleOwner}, "admin", store.OrgRoleAdmin, 0)
		Expect(err).NotTo(HaveOccurred())
		st, raw = e.call(ctx, adminTok.Secret, http.MethodGet, "/audit/export?scope=install", nil)
		Expect(st).To(Equal(http.StatusForbidden), string(raw))

		st, _ = e.call(ctx, e.token, http.MethodGet, "/audit/export?scope=all", nil)
		Expect(st).To(Equal(http.StatusBadRequest))
		st, _ = e.call(ctx, e.token, http.MethodGet, "/audit/export?fromSeq=0", nil)
		Expect(st).To(Equal(http.StatusBadRequest))
		st, raw = e.call(ctx, e.token, http.MethodGet, "/audit/export?fromSeq=1000000", nil)
		Expect(st).To(Equal(http.StatusBadRequest), string(raw))
		st, _ = e.call(ctx, "", http.MethodGet, "/audit/export", nil)
		Expect(st).To(Equal(http.StatusUnauthorized))
	})

	It("exports the install chain to a token rooted at a platform admin, checked on every request", func(ctx SpecContext) {
		e := newEnv(ctx)
		_, err := audit.InTx(ctx, pool, func(_ *store.Queries, b audit.Batch) error {
			b.Install(audit.InstallEvent{
				Actor: audit.System("koinon-test:spec"), Action: "platform_price.set", TargetKind: "platform_price", TargetID: "adminapi-install-export",
			})
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `INSERT INTO platform_admins (user_id) VALUES ($1)`, e.user)
		Expect(err).NotTo(HaveOccurred())

		st, raw := e.call(ctx, e.token, http.MethodGet, "/audit/export?scope=install", nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))
		res, err := audit.VerifyExport(bytes.NewReader(raw), audit.ExportVerifyOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Break).To(BeNil())
		Expect(res.Header.Chain).To(Equal("install"))
		Expect(string(raw)).To(ContainSubstring("adminapi-install-export"))

		By("an org parameter has no meaning for the install chain")
		st, _ = e.call(ctx, e.token, http.MethodGet, "/audit/export?scope=install&org="+e.org.String(), nil)
		Expect(st).To(Equal(http.StatusBadRequest))

		By("the access ends with the admin")
		_, err = pool.Exec(ctx, `DELETE FROM platform_admins WHERE user_id = $1`, e.user)
		Expect(err).NotTo(HaveOccurred())
		st, _ = e.call(ctx, e.token, http.MethodGet, "/audit/export?scope=install", nil)
		Expect(st).To(Equal(http.StatusForbidden))
	})

	// get starts an export request and returns its response without reading the body.
	get := func(ctx context.Context, e *env) (*http.Response, error) {
		GinkgoHelper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.srv.URL+"/admin/v1/audit/export", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+e.token)
		return http.DefaultClient.Do(req)
	}

	It("ends the export when the client goes away mid-stream, leaving no goroutine behind", func(ctx SpecContext) {
		e := newEnv(ctx)
		exported := make(chan error, 1)
		ended := make(chan struct{})
		// An export longer than every buffer between the two ends: it only stops when a write fails.
		adminapi.SetAuditExporter(e.api, func(_ context.Context, w io.Writer, _ audit.ExportScope, _ int64) error {
			line := []byte(strings.Repeat("x", 1023) + "\n")
			for {
				if _, err := w.Write(line); err != nil {
					exported <- err
					return err
				}
			}
		}, func() { close(ended) })

		resp, err := get(ctx, e)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		_, err = io.ReadFull(resp.Body, make([]byte, 16))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Body.Close()).To(Succeed(), "the client disconnects after a few bytes")

		Eventually(exported).WithTimeout(30*time.Second).Should(Receive(MatchError(io.ErrClosedPipe)), "the export's write fails and it returns")
		Eventually(ended).WithTimeout(10*time.Second).Should(BeClosed(), "the export goroutine ends")
	})

	It("aborts the connection when the export fails after the first byte, instead of ending a truncated body cleanly", func(ctx SpecContext) {
		e := newEnv(ctx)
		ended := make(chan struct{})
		adminapi.SetAuditExporter(e.api, func(_ context.Context, w io.Writer, _ audit.ExportScope, _ int64) error {
			line := []byte(`{"chain_seq":1}` + strings.Repeat(" ", 1000) + "\n")
			for range 64 {
				if _, err := w.Write(line); err != nil {
					return err
				}
			}
			return errors.New("koinon-test: the database went away")
		}, func() { close(ended) })

		resp, err := get(ctx, e)
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			var raw []byte
			raw, err = io.ReadAll(resp.Body)
			Expect(string(raw)).NotTo(ContainSubstring(`"error"`), "no error object is appended to the stream")
			Expect(raw).NotTo(BeEmpty(), "the stream had started")
		}
		Expect(err).To(HaveOccurred(), "the client sees a broken transfer, not a clean end")
		Eventually(ended).WithTimeout(10 * time.Second).Should(BeClosed())

		// exportCalls returns the summaries of the admin_api.call rows of the export path.
		exportCalls := func(ctx context.Context) []string {
			rows, err := store.New(pool).ListAuditEvents(ctx, store.ListAuditEventsParams{OrgID: e.org, PageSize: 100})
			Expect(err).NotTo(HaveOccurred())
			var calls []string
			for _, r := range rows {
				if r.Action == "admin_api.call" && strings.HasSuffix(r.TargetID, "/audit/export") {
					calls = append(calls, string(r.Summary))
				}
			}
			return calls
		}
		By("the aborted call is audited as aborted")
		// The row is written after the handler returns, which can be after the client saw the abort.
		Eventually(exportCalls).WithContext(ctx).WithTimeout(10 * time.Second).Should(ConsistOf(
			And(ContainSubstring(`"aborted": true`), ContainSubstring(`"status": 200`)),
		))

		By("an ordinary error before the first byte is still an error response")
		adminapi.SetAuditExporter(e.api, func(context.Context, io.Writer, audit.ExportScope, int64) error {
			return audit.ErrBadFilter
		}, nil)
		st, raw := e.call(ctx, e.token, http.MethodGet, "/audit/export", nil)
		Expect(st).To(Equal(http.StatusBadRequest), string(raw))
		Expect(string(raw)).To(ContainSubstring(`"error"`))

		By("and a call that was not aborted has no aborted key")
		Eventually(exportCalls).WithContext(ctx).WithTimeout(10 * time.Second).Should(HaveLen(2))
		Expect(exportCalls(ctx)).To(ContainElement(And(ContainSubstring(`"status": 400`), Not(ContainSubstring("aborted")))))
	})
})
