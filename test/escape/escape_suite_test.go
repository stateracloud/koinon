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
// them. Runs on the home lab only (make test-escape):
//
//	KOINON_OPENSHELL_ENDPOINT   gateway host:port, for example a port-forward (required)
//	KOINON_OPENSHELL_MTLS_DIR   directory with tls.crt, tls.key and ca.crt (required)
//	KOINON_OPENSHELL_IMAGE      sandbox image with sh, git, curl and python3 at /usr/bin
//	                            (required; the spike probe image qualifies, the flavor images
//	                            do not)
//	KOINON_OPENSHELL_WORKSPACE  workspace to use (default koinon-escape)
//	KOINON_ESCAPE_REPO          the repository the grant is bound to (default
//	                            octocat/Hello-World)
//	KOINON_ESCAPE_OTHER_REPO    a repository it is not bound to (default octocat/Spoon-Knife)
//	KOINON_ESCAPE_CLUSTER_DNS   the cluster DNS Service IP (default 10.96.0.10)
//	KOINON_ESCAPE_EGRESS, KOINON_ESCAPE_METADATA, KOINON_ESCAPE_API
//	                            comma-separated host:port lists that replace the default
//	                            targets of the egress, metadata and API server cases
//
// Provider records hold dummy values, not real credentials: every request these specs make
// outside the grant must be refused by the sandbox before it leaves the sandbox, and a refusal
// by the far end (a GitHub 401, a timeout) fails the spec.
package escape_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestEscape(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Escape Suite")
}
