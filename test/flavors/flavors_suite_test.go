// SPDX-License-Identifier: Apache-2.0

// Package flavors_test is the flavor contract suite (ADR 0025, 0035; plan items F1–F3, E3). It
// runs each flavor image with Docker the way OpenShell runs it in a sandbox: an idle container
// as UID 10001 with a read-only root file system and the home on a volume, and the harness
// started with a non-TTY exec, speaking ACP on stdin and stdout through internal/acp. The
// harness talks to an in-process fake model server and a minimal MCP server standing in for
// agentgateway.
//
// Plain Docker has no OpenShell placeholder substitution, so the container gets a fake key
// directly. No real key or paid model is ever used.
//
// The suite skips (testenv.Require) when Docker is unavailable and fails when a flavor image is missing. Images are
// tagged per source tree (koinon/flavor-<name>:dev-<hash>, hack/flavor-tag.sh), so build them from
// this tree with `make flavors` (or set KOINON_FLAVOR_BUILD=1 to build them here). Override an
// image with KOINON_FLAVOR_IMAGE_<NAME>, for example KOINON_FLAVOR_IMAGE_OPENCODE, and print every
// harness's stderr with KOINON_FLAVOR_STDERR=1.
//
// The specs are the same for every flavor (contract_test.go, agentd_test.go); where harnesses
// legitimately differ (session/resume, which tool calls ask for permission, the unknown-session
// error) the flavor's entry (opencode_test.go) says what to expect.
package flavors_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestFlavors(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "flavor contract")
}
