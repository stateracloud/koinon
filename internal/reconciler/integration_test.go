// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/testing/testenv"
	"github.com/stateracloud/koinon/pricing"
)

// The integration test reconciles one agent against a real OpenShell gateway, only when
// KOINON_OPENSHELL_ENDPOINT is set (host:port, for example a port-forward), with:
//
//	KOINON_OPENSHELL_MTLS_DIR   directory with tls.crt, tls.key and ca.crt (required)
//	KOINON_OPENSHELL_IMAGE      harness image for the sandbox (required)
//	KOINON_AGENTGATEWAY_HOST    agentgateway host the provider profile binds the key to
//	                            (default agentgateway.agentgateway.svc.cluster.local, port 8080)
//	KOINON_E2E_ORG_ID           fixed org UUID, so every run reuses one long-lived workspace
//	                            (kw-… is derived from it); default a new org per run
//
// It creates a workspace (kw-…), a provider record and a sandbox, waits for the sandbox to be
// ready, stops it as idle, then archives the agent and waits for the sandbox to be deleted. The
// workspace is left behind (OpenShell 0.1.5 has no workspace delete in Koinon's backend), which
// is why an external cluster run sets KOINON_E2E_ORG_ID.
var _ = Describe("against an OpenShell gateway", Label("integration"), func() {
	It("activates, stops and archives an agent", func(ctx SpecContext) {
		testenv.Require(testenv.Var("openshell", "KOINON_OPENSHELL_ENDPOINT"))
		endpoint := os.Getenv("KOINON_OPENSHELL_ENDPOINT")
		dir, image := os.Getenv("KOINON_OPENSHELL_MTLS_DIR"), os.Getenv("KOINON_OPENSHELL_IMAGE")
		Expect(dir).NotTo(BeEmpty(), "KOINON_OPENSHELL_MTLS_DIR")
		Expect(image).NotTo(BeEmpty(), "KOINON_OPENSHELL_IMAGE")
		host := os.Getenv("KOINON_AGENTGATEWAY_HOST")
		if host == "" {
			host = "agentgateway.agentgateway.svc.cluster.local"
		}

		backend, err := openshell.Dial(openshell.Config{
			Endpoint: endpoint,
			CertFile: filepath.Join(dir, "tls.crt"),
			KeyFile:  filepath.Join(dir, "tls.key"),
			CAFile:   filepath.Join(dir, "ca.crt"),
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(backend.Close)

		var later time.Time
		r, err := reconciler.New(pool, backend, &fakeModelGateway{},
			reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace},
			reconciler.Config{
				Prices:  prices.NewResolver(pricing.Embedded()),
				Gateway: reconciler.Endpoint{Host: host, Port: 8080},
				Harnesses: map[string]reconciler.Harness{
					harness.OpenCode: {Image: image, Binaries: reconciler.DefaultBinaries[harness.OpenCode]},
				},
				Logger: slog.New(slog.NewTextHandler(GinkgoWriter, nil)),
				Now: func() time.Time {
					if !later.IsZero() {
						return later
					}
					return time.Now()
				},
			})
		Expect(err).NotTo(HaveOccurred())

		org, user := newOrg(ctx)
		if id := os.Getenv("KOINON_E2E_ORG_ID"); id != "" {
			org, user = newOrgWithID(ctx, id)
		}
		agent := newAgent(ctx, org, user)
		names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
		ws := execution.WorkspaceName(org.Bytes)
		DeferCleanup(func(ctx context.Context) error {
			if _, err := backend.DeleteSandbox(ctx, ws, names.Sandbox); err != nil {
				return err
			}
			return backend.DeleteProvider(ctx, ws, names.Provider)
		})

		phase := func(g Gomega) execution.Phase {
			_ = r.ReconcileAll(ctx)
			sb, err := backend.GetSandbox(ctx, ws, names.Sandbox)
			if errors.Is(err, execution.ErrNotFound) {
				return "NotFound"
			}
			g.Expect(err).NotTo(HaveOccurred())
			return sb.Phase
		}
		Eventually(phase).WithContext(ctx).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).
			Should(Equal(execution.PhaseReady))

		later = time.Now().Add(time.Hour)
		Eventually(phase).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).
			Should(Equal(execution.PhaseStopped))

		_, err = q.ArchiveAgent(ctx, store.ArchiveAgentParams{OrgID: org, ID: agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Eventually(phase).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).
			Should(Equal(execution.Phase("NotFound")))
	}, NodeTimeout(12*time.Minute))
})
