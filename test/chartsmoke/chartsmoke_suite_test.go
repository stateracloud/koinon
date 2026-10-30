// SPDX-License-Identifier: Apache-2.0

// Package chartsmoke_test is the rendered startup smoke test (regressions like #284, where the
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
// Covered: every ci values file with postgres.mode=external (external-kubernetes and
// external-standalone: both gateway modes). The cnpg files are not run: their DATABASE_URL is
// sslmode=verify-full against the CNPG cluster's CA, so a Postgres container would need a CA and
// a server certificate of its own to test the TLS set-up, not the startup contract; the Koinon
// containers' args, env and mounts come from the same templates in both. Not run either:
// koinon-gateway (agentgateway, third-party). koinon-preflight inspects the platform (Agent
// Sandbox, OpenShell, Kata, the gateway controller) that envtest does not have, so it must run
// its checks and report (it fails them), without a flag error or a Forbidden answer. The
// reconciler and agentd get an OpenShell address that nothing listens on: they need to start and
// serve their probes, not to talk to a gateway (see smokeValues).
//
// What it proves, for example: reverting the migrateOnly check in cmd/koinon-api/main.go (#285)
// makes "runs the migrate Job to completion" fail with "--otlp-token-dir is required with
// --otlp-addr", the install failure of #284.
//
// Needs Docker, helm, Go (to build the images) and KUBEBUILDER_ASSETS (setup-envtest, as for
// the admission suite); `make test-chart-smoke` sets it up. Skips without Docker or
// KUBEBUILDER_ASSETS, like the other suites that need them.
package chartsmoke_test

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

func TestChartSmoke(t *testing.T) {
	RegisterFailHandler(Fail)
	ctrllog.SetLogger(logr.Discard())
	RunSpecs(t, "Chart startup smoke suite")
}

var images map[string]string

var _ = BeforeSuite(func(ctx SpecContext) {
	if envOr("KUBEBUILDER_ASSETS", "") == "" {
		Skip("KUBEBUILDER_ASSETS is not set (setup-envtest use -p path; make test-chart-smoke)")
	}
	if !dockerAvailable(ctx) {
		Skip("Docker is not available")
	}
	images = buildImages(ctx, "../..")
	Expect(images).To(HaveLen(len(koinonImages)))
}, NodeTimeout(10*60*1e9))

func dockerAvailable(ctx context.Context) bool {
	p, err := testcontainers.NewDockerProvider()
	if err != nil {
		return false
	}
	defer func() { _ = p.Close() }()
	return p.Health(ctx) == nil
}
