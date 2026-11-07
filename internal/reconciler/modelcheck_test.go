// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/modelcheck"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
	"github.com/stateracloud/koinon/pricing"
)

// The provider echo check end to end (ADR 0056 amendment): the reconciler issues the org's
// model-check key, koinon-api's service sends the check through a "gateway" (the fake model server)
// with that key, the ledger records the request as a system entry, the report shows it, and the
// key's budget is enforced.
var _ = Describe("The provider echo check end to end", func() {
	const secretName = "koinon-gateway-providers"

	It("issues the key, runs the check with it, records a system entry that counts against the budgets, and stops at the daily budget", func(ctx SpecContext) {
		osg := fakegateway.New()
		cc, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = cc.Close(); osg.Stop() })
		mg := &fakeModelGateway{}
		kube := fake.NewClientset()
		r, err := reconciler.New(pool, openshell.New(cc), mg, reconciler.KubeSecrets{Client: kube, Namespace: namespace}, reconciler.Config{
			Gateway:        reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Keys:           ring,
			ProviderKeys:   reconciler.KubeProviderKeys{Client: kube, Namespace: namespace, Name: secretName},
			ProviderKeyDir: "/secrets/providers", URLPolicy: testPolicy, Prices: prices.NewResolver(pricing.Embedded()),
			Logger: slog.New(slog.DiscardHandler),
		})
		Expect(err).NotTo(HaveOccurred())
		org, user := newOrg(ctx)
		p := addProvider(ctx, org, user, "openrouter", "", "fake/echo")
		DeferCleanup(func(ctx SpecContext) {
			_ = catalog.New(pool, ring, nil).DeleteProvider(ctx, org, audit.System("koinon-test:spec"), p.ID)
		})
		var slug string
		Expect(pool.QueryRow(ctx, `SELECT slug FROM orgs WHERE id = $1`, org).Scan(&slug)).To(Succeed())
		check := gateway.AgentRef{Org: slug, Name: gateway.ModelCheckAgentName}

		By("the reconciler issues the key")
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		sec, err := kube.CoreV1().Secrets(namespace).Get(ctx, mcpservers.ModelCheckSecretName(org), metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		material := string(sec.Data[mcpservers.ExecKeyDataKey])
		Expect(material).NotTo(BeEmpty())

		By("the check runs through the gateway with that key, on one model of the provider")
		fake, err := fakemodel.New(fakemodel.Script{Model: "fake/echo", Turns: []fakemodel.Turn{{Text: modelcheck.Line}}})
		Expect(err).NotTo(HaveOccurred())
		var auth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			auth = req.Header.Get("Authorization")
			fake.ServeHTTP(w, req)
		}))
		DeferCleanup(srv.Close)
		svc := modelcheck.New(pool, modelcheck.GatewayChat{
			URL: srv.URL, Keys: mcpservers.KubeExecKeys{Client: kube, Namespace: namespace, SecretName: mcpservers.ModelCheckSecretName},
		}, prices.NewResolver(pricing.Embedded()))
		res, err := svc.Check(ctx, org, providerID(p.ID), "fake/echo", audit.System("koinon-test:spec"), user)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Unchanged).To(BeTrue())
		Expect(auth).To(Equal("Bearer " + material))
		Expect(fake.Requests()).To(HaveLen(1))

		By("the gateway's access log record becomes a system entry: no agent, no run")
		var keyID string
		for _, k := range mg.last().Keys {
			if k.Agent == check {
				keyID = k.ID
			}
		}
		ing := &ledger.Ingester{DB: pool, Prices: prices.NewResolver(pricing.Embedded())}
		entry := func(id string, in int64) ledger.Entry {
			return ledger.Entry{
				RequestID: id, KeyID: keyID, Agent: check, Model: "fake/echo",
				InputTokens: in, InputKnown: true, OutputTokens: 0, OutputKnown: true, Time: time.Now(),
			}
		}
		got, err := ing.Ingest(ctx, []ledger.Entry{entry("check-1", 50_000)})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Recorded).To(Equal(1))
		var principal string
		var agentNull, runNull bool
		Expect(pool.QueryRow(ctx, `SELECT principal, agent_id IS NULL, run_id IS NULL FROM spend_entries WHERE org_id = $1`, org).Scan(&principal, &agentNull, &runNull)).To(Succeed())
		Expect(principal).To(Equal("system"))
		Expect(agentNull && runNull).To(BeTrue())
		again, err := ing.Ingest(ctx, []ledger.Entry{entry("check-1", 50_000)})
		Expect(err).NotTo(HaveOccurred())
		Expect(again.Duplicates).To(Equal(1), "a replay records nothing")

		By("the report shows it as system checks, apart from the agents' models")
		q := store.New(pool)
		since := budget.MonthStart(time.Now())
		sys, err := q.SpendSystemSince(ctx, store.SpendSystemSinceParams{OrgID: org, Since: pgtype.Timestamptz{Time: since, Valid: true}})
		Expect(err).NotTo(HaveOccurred())
		Expect(sys.Requests).To(Equal(int64(1)))
		Expect(money.ParseNumeric(sys.CostUsd)).To(Equal(money.FromCents(5)))
		rows, err := q.SpendReport(ctx, store.SpendReportParams{OrgID: org, Since: pgtype.Timestamptz{Time: since, Valid: true}})
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(BeEmpty())

		By("it counts against the org's monthly cap")
		cap5 := money.FromCents(50)
		Expect(budget.New(pool).SetOrgCap(ctx, org, audit.System("koinon-test:spec"), &cap5)).To(Succeed())
		st, err := budget.ComputeSystem(ctx, q, org, time.Now())
		Expect(err).NotTo(HaveOccurred())
		var orgSpent money.USD
		for _, l := range st.Levels {
			if l.Name == budget.OrgMonthly {
				orgSpent = l.Spent
			}
		}
		Expect(orgSpent).To(Equal(money.FromCents(5)))

		By("the key's daily budget is enforced: spent, the amount pushed is zero")
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		Expect(mg.last().Limits[check].Amount).To(Equal(money.MustParse("0.05")))
		_, err = ing.Ingest(ctx, []ledger.Entry{entry("check-2", 50_000)})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		Expect(mg.last().Limits[check].Amount).To(BeZero())
	})
})

func providerID(s string) pgtype.UUID {
	var id pgtype.UUID
	Expect(id.Scan(s)).To(Succeed())
	return id
}
