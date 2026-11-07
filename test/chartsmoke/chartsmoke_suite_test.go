// SPDX-License-Identifier: Apache-2.0

// Package chartsmoke_test is the rendered startup smoke test (regressions like old issue 284, where the
// chart's migrate Job failed on a fresh install because a new required flag had no token mounted
// into that Job). Unit tests call the flag parser, and hack/chart-check.sh only renders YAML;
// nothing ran the real binary with the chart's real command line. This does:
//
//   - helm renders the chart for each charts/koinon/ci values file whose database can run here,
//   - the Koinon images are built from the working tree,
//   - the rendered Jobs and Deployments start as containers, with the rendered args, env,
//     volumes and mounts, against a Postgres container and an envtest API server that enforces
//     the chart's RBAC (see runner for what is and is not modelled).
//
// Covered: every charts/koinon/ci values file, with the database overridden to external (the
// cnpg files' DATABASE_URL is sslmode=verify-full against the CNPG cluster's CA; a Postgres
// container with a CA of its own would test that TLS set-up, not the startup contract, and the
// rest of the pod specs does not depend on the database mode). The operator-supplied Secrets the
// pods reference (database URIs, the OpenShell client certificate, OIDC client secret, existing
// KEK, bootstrap-admin and OTLP token Secrets, the checkpoints' S3 credentials) are created with
// fake data, so the mounts and keys they reach start for real. Helm's order is modelled: pre-install
// hooks by weight, then the other objects, then the Deployments. Not run: koinon-gateway
// (agentgateway, third-party). koinon-preflight inspects the platform (Agent Sandbox, OpenShell,
// Kata, the gateway controller) that envtest does not have, so it must run its checks and report
// (it fails them), without a flag error or a Forbidden answer. The reconciler and agentd get an
// OpenShell address that nothing listens on: they need to start and serve their probes, not to talk
// to a gateway. After their probes answer, the Deployments run for a few more seconds and must
// still be running and free of "forbidden" in their logs (RBAC rules the chart lacks fail there).
//
// The envtest API server listens on all interfaces, because the containers reach the host through
// host-gateway, which on Docker Desktop and OrbStack is not an address of a host interface. It is
// short-lived and only answers authenticated requests (RBAC).
//
// What it proves, for example: moving the serve-only OTLP token check from serve to the root of
// internal/cmd/koinonapi (old issue 285) makes "runs the migrate Job to completion" fail with "--otlp-token-dir is required with
// --otlp-addr", the install failure of old issue 284.
//
// Needs Docker, helm, Go (to build the images) and KUBEBUILDER_ASSETS (setup-envtest, as for
// the admission suite); `make test-chart-smoke` sets it up. Skips without Docker or
// KUBEBUILDER_ASSETS, like the other suites that need them.
package chartsmoke_test

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

func TestChartSmoke(t *testing.T) {
	RegisterFailHandler(Fail)
	ctrllog.SetLogger(logr.Discard())
	RunSpecs(t, "Chart startup smoke suite")
}

var images map[string]string

var _ = BeforeSuite(func(ctx SpecContext) {
	testenv.Require(testenv.Envtest, testenv.Docker)
	images = buildImages(ctx, "../..")
	DeferCleanup(func(ctx context.Context) { removeImages(ctx, images) })
	Expect(images).To(HaveLen(len(koinonImages)))
}, NodeTimeout(10*time.Minute))
