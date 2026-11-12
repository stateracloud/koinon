// SPDX-License-Identifier: Apache-2.0

// Package harnesses_test is the harness contract suite (ADR 0025, 0035; plan items F1–F3, E3). It
// runs each harness image with Docker the way OpenShell runs it in a sandbox: an idle container
// as UID 10001 with a read-only root file system and the home on a volume, and the harness
// started with a non-TTY exec, speaking ACP on stdin and stdout through internal/acp. The
// harness talks to an in-process fake model server and a minimal MCP server standing in for
// agentgateway.
//
// Plain Docker has no OpenShell placeholder substitution, so the container gets a fake key
// directly. No real key or paid model is ever used.
//
// The suite skips (testenv.Require) when Docker is unavailable and fails when a harness image is missing. Images are
// tagged per source tree (koinon/sandbox:dev-<hash>, hack/harness-tag.sh), so build them from
// this tree with `make harnesses` (or set KOINON_HARNESS_BUILD=1 to build them here). Override an
// image with KOINON_HARNESS_IMAGE_<NAME>, for example KOINON_HARNESS_IMAGE_OPENCODE, and print every
// harness's stderr with KOINON_HARNESS_STDERR=1.
//
// The specs are the same for every harness (contract_test.go, agentd_test.go); where harnesses
// legitimately differ (session/resume, which tool calls ask for permission, the unknown-session
// error) the harness's entry (opencode_test.go) says what to expect.
package harnesses_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestHarnesses(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "harness contract")
}
