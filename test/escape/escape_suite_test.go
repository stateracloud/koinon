// SPDX-License-Identifier: Apache-2.0

// Package escape_test is the sandbox escape suite (ADR 0025, plan item E2). It has two parts,
// each selected by a label and skipped without its prerequisites.
//
// Label "admission": the chart's ValidatingAdmissionPolicies, rendered with helm and applied to
// a real API server from envtest. Runs anywhere, and in CI (make test-admission):
//
//	KUBEBUILDER_ASSETS  envtest binaries, from `setup-envtest use 1.37.0 -p path` (required)
//	HELM                helm binary (default helm on PATH)
//
// Label "openshell": sandboxes on a real OpenShell gateway, rendered as the reconciler renders
// them. Runs on kind (make test-escape-kind, which sets the variables below) and on any other
// cluster's gateway (make test-escape):
//
//	KOINON_OPENSHELL_ENDPOINT   gateway host:port, for example a port-forward (required)
//	KOINON_OPENSHELL_MTLS_DIR   directory with tls.crt, tls.key and ca.crt (required)
//	KOINON_OPENSHELL_IMAGE      sandbox image with sh, git, curl and python3 at /usr/bin
//	                            (required; the spike probe image qualifies, the harness images
//	                            do not)
//	KOINON_OPENSHELL_WORKSPACE  workspace to use (default koinon-escape)
//	KOINON_ESCAPE_REPO          the repository the grant is bound to (default
//	                            octocat/Hello-World)
//	KOINON_ESCAPE_OTHER_REPO    a repository it is not bound to (default octocat/Spoon-Knife)
//	KOINON_ESCAPE_GATEWAY_HOST  a non-loopback name the proxy can resolve, with a server on port
//	                            4000 that answers every request 418 and X-Koinon-Standin: 1,
//	                            standing in for agentgateway: escape-gateway-standin in
//	                            test/fixtures/rt1/manifests (no default; the gateway case is
//	                            skipped without it)
//	KOINON_ESCAPE_GITEA_URL     the Gitea base URL the read grant is rendered for (default
//	                            https://example.com/gitea; the host must resolve in the sandbox and accept
//	                            a TLS connect on 443: kind.sh points it at escape-gitea-standin, old issue 371)
//	KOINON_ESCAPE_CLUSTER_DNS   the cluster DNS Service IP (kube-dns); no default, the DNS spec skips
//	                            (fails under KOINON_REQUIRE=escape-cluster-ips) without it
//	KOINON_ESCAPE_API, KOINON_ESCAPE_INTERNAL
//	                            comma-separated host:port lists of the API server and platform
//	                            service targets; no default, the specs skip likewise without them.
//	                            test/escape/kind.sh reads the Service IPs from the cluster it targets
//	KOINON_ESCAPE_REGISTRIES    comma-separated host:port list of the kind local registry and its
//	                            caches (test/e2e/kind/registry.sh); test/escape/kind.sh reads them
//	                            from Docker; the case skips without it
//	KOINON_ESCAPE_EGRESS, KOINON_ESCAPE_METADATA
//	                            comma-separated host:port lists that replace the default
//	                            targets of the egress and metadata cases
//	KOINON_ESCAPE_HOST_KERNEL   the nodes' uname -r; enables the Kata guest case (a manual
//	                            release gate); the case is skipped without it
//	KOINON_ESCAPE_NAMESPACE     the workspace's Kubernetes namespace; enables the live
//	                            admission case and the NetworkPolicy bypass case (spike 8.15),
//	                            with KOINON_ESCAPE_KUBECONFIG and KOINON_ESCAPE_KUBE_CONTEXT
//	                            (default $KUBECONFIG, current context), KOINON_ESCAPE_POLICY
//	                            (the chart's pods policy), KOINON_ESCAPE_NETPOL (the chart's
//	                            default-deny NetworkPolicy name) and KOINON_ESCAPE_SERVICES
//	                            (namespace/name:port list whose ClusterIPs must be unreachable).
//	                            When KOINON_ESCAPE_SERVICES is set, every listed Service must
//	                            exist (a missing one fails the spec); the built-in default list
//	                            is skipped where a Service is absent.
//	                            The live admission case skips unless the chart's policy is bound
//	                            to the namespace. The Kata case fails when the variable is set
//	                            and the sandbox is not under Kata.
//
// Provider records hold dummy values, not real credentials: every request these specs make
// outside the grant must be refused by the sandbox before it leaves the sandbox, and a refusal
// by the far end (a GitHub 401, a timeout) fails the spec.
package escape_test

import (
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestEscape(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Escape Suite")
}

var _ = AfterSuite(func(ctx SpecContext) { deleteWorkspace(ctx) }, NodeTimeout(2*time.Minute))
