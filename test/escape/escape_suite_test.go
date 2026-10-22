// SPDX-License-Identifier: Apache-2.0

// Package escape_test is the sandbox escape suite (ADR 0025, plan item E2). Its specs run
// against a real OpenShell gateway only, and skip without one:
//
//	KOINON_OPENSHELL_ENDPOINT   gateway host:port, for example a port-forward (required)
//	KOINON_OPENSHELL_MTLS_DIR   directory with tls.crt, tls.key and ca.crt (required)
//	KOINON_OPENSHELL_IMAGE      sandbox image with sh, git and curl at /usr/bin (required;
//	                            the opencode flavor image qualifies)
//	KOINON_OPENSHELL_WORKSPACE  workspace to use (default koinon-escape)
//	KOINON_ESCAPE_REPO          the repository the grant is bound to (default
//	                            octocat/Hello-World)
//	KOINON_ESCAPE_OTHER_REPO    a repository it is not bound to (default octocat/Spoon-Knife)
//
// The provider record holds a dummy value, not a GitHub token: every request these specs make
// must be refused by the sandbox's policy before it leaves the sandbox, and a refusal by GitHub
// itself (401, 404) fails the spec.
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
