// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	"github.com/stateracloud/koinon/internal/gateway"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/pricing"
)

var _ = Describe("Reconciler metrics", func() {
	var (
		osg  *fakegateway.Gateway
		mg   *fakeModelGateway
		reg  *prometheus.Registry
		m    *reconciler.Metrics
		r    *reconciler.Reconciler
		org  pgtype.UUID
		user pgtype.UUID
	)

	BeforeEach(func(ctx SpecContext) {
		osg = fakegateway.New()
		conn, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = conn.Close(); osg.Stop() })
		mg = &fakeModelGateway{}
		kube := fake.NewClientset()
		reg = prometheus.NewRegistry()
		m = reconciler.NewMetrics(reg)
		org, user = newOrg(ctx)
		addProvider(ctx, org, user, "upstream", "", "fake")
		r, err = reconciler.New(pool, openshell.New(conn), mg, reconciler.KubeSecrets{Client: kube, Namespace: namespace},
			reconciler.Config{
				Prices:         prices.NewResolver(pricing.Embedded()),
				Gateway:        reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
				Keys:           ring,
				ProviderKeys:   reconciler.KubeProviderKeys{Client: kube, Namespace: namespace, Name: "koinon-gateway-providers"},
				ProviderKeyDir: "/secrets/providers",
				URLPolicy:      testPolicy,
				Harnesses: map[string]reconciler.Harness{
					harness.OpenCode: {Image: image, Binaries: reconciler.DefaultBinaries[harness.OpenCode]},
				},
				RuntimeClassName: "kata",
				Logger:           slog.New(slog.DiscardHandler),
				Metrics:          m,
			})
		Expect(err).NotTo(HaveOccurred())
	})

	It("counts a pass, observes its durations and counts the sandboxes by state", func(ctx SpecContext) {
		newAgent(ctx, org, user)
		Expect(r.ReconcileAll(ctx)).To(Succeed())

		Expect(testutil.ToFloat64(m.Passes.WithLabelValues("ok"))).To(Equal(1.0))
		Expect(testutil.ToFloat64(m.Passes.WithLabelValues("error"))).To(Equal(0.0))
		Expect(testutil.CollectAndCount(m.PassDuration)).To(Equal(1))
		Expect(testutil.CollectAndCount(m.GatewayApply)).To(Equal(1))
		count, err := testutil.GatherAndCount(reg, "koinon_reconciler_pass_duration_seconds", "koinon_reconciler_gateway_apply_duration_seconds")
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(2))
		Expect(testutil.ToFloat64(m.Sandboxes.WithLabelValues("ready"))).To(BeNumerically(">=", 1), "the database is shared by the specs")
		Expect(testutil.ToFloat64(m.AgentsHeldBack)).To(Equal(0.0))
	})

	It("counts a failed gateway apply as an error pass of kind gateway_apply", func(ctx SpecContext) {
		newAgent(ctx, org, user)
		mg.fail = errors.New("gateway unreachable")
		Expect(r.ReconcileAll(ctx)).To(HaveOccurred())

		Expect(testutil.ToFloat64(m.Passes.WithLabelValues("error"))).To(Equal(1.0))
		Expect(testutil.ToFloat64(m.PassErrors.WithLabelValues("gateway_apply"))).To(Equal(1.0))
		Expect(testutil.ToFloat64(m.PassErrors.WithLabelValues("gateway_rejected"))).To(Equal(0.0))
		Expect(testutil.ToFloat64(m.AgentsHeldBack)).To(Equal(1.0), "the new agent's key is not live")
	})

	It("counts a refused config as kind gateway_config, raises the rejected gauge and clears it on the next good pass", func(ctx SpecContext) {
		newAgent(ctx, org, user)
		mg.fail = &gateway.ConfigRejectedError{Revision: "abc", Reason: "USD budget limits support at most 9 fractional digits"}
		Expect(r.ReconcileAll(ctx)).To(MatchError(gateway.ErrConfigRejected))

		Expect(testutil.ToFloat64(m.PassErrors.WithLabelValues("gateway_config"))).To(Equal(1.0))
		Expect(testutil.ToFloat64(m.PassErrors.WithLabelValues("gateway_apply"))).To(Equal(0.0), "a refusal is not an unreachable gateway")
		Expect(testutil.ToFloat64(m.GatewayConfigRejected)).To(Equal(1.0))
		Expect(testutil.ToFloat64(m.AgentsHeldBack)).To(Equal(1.0))

		mg.fail = nil
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		Expect(testutil.ToFloat64(m.GatewayConfigRejected)).To(Equal(0.0))
		Expect(testutil.ToFloat64(m.PassErrors.WithLabelValues("gateway_config"))).To(Equal(1.0), "a counter does not go back")
	})

	It("counts sandbox repairs and agents marked unhealthy", func(ctx SpecContext) {
		agent := newAgent(ctx, org, user)
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		row, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: org, AgentID: agent.ID})
		Expect(err).NotTo(HaveOccurred())
		names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)

		for range 3 { // ADR 0054: three recreations in 24 hours are allowed
			osg.SetPhase(row.Workspace, names.Sandbox, openshellv1.SandboxPhase_SANDBOX_PHASE_ERROR)
			Expect(r.ReconcileAll(ctx)).To(Succeed()) // deletes
			Expect(r.ReconcileAll(ctx)).To(Succeed()) // creates
		}
		Expect(testutil.ToFloat64(m.SandboxRepairs)).To(Equal(3.0))
		Expect(testutil.ToFloat64(m.AgentsUnhealthy)).To(Equal(0.0))

		osg.SetPhase(row.Workspace, names.Sandbox, openshellv1.SandboxPhase_SANDBOX_PHASE_ERROR)
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		Expect(testutil.ToFloat64(m.SandboxRepairs)).To(Equal(3.0))
		Expect(testutil.ToFloat64(m.AgentsUnhealthy)).To(Equal(1.0))
	})

	It("sets a sandbox state's gauge back to 0 when the state empties", func(ctx SpecContext) {
		m.Sandboxes.WithLabelValues("error").Set(7) // stale value from an earlier pass
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		Expect(testutil.ToFloat64(m.Sandboxes.WithLabelValues("error"))).To(Equal(
			func() float64 {
				var n float64
				Expect(pool.QueryRow(ctx, `SELECT count(*) FROM agent_sandboxes WHERE state = 'error'`).Scan(&n)).To(Succeed())
				return n
			}()))
	})
})
