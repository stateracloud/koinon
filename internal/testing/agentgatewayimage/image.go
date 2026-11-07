// SPDX-License-Identifier: Apache-2.0

// Package agentgatewayimage names the agentgateway image the Go tests start (#238). It is the image
// the chart pins (gateway.standalone.image in charts/koinon/values.yaml), by digest, so a test runs
// what an install runs; hack/agentgateway-pin.sh --check fails when [Upstream] differs from the pin.
//
// CI pulls the copy in GHCR that .github/workflows/mirror-agentgateway-test-image.yml keeps, so an
// outage of an upstream registry does not turn every Go job red. The go-test job logs in, pulls the
// copy once and sets [MirrorVar] to the copy's repository; everywhere else (a laptop, an external cluster, a
// fork's own CI without access to the copy) the tests pull the chart's repository, which needs no
// credentials. docs/dev/ci.md, "The agentgateway test image".
package agentgatewayimage

import "os"

const (
	// Digest is the chart's gateway.standalone.image digest.
	Digest = "sha256:60216aafb03d37f94e952ff3d51d2f630a65b10ecaebdc0a864882f92610c45b"
	// Upstream is [Digest] in the repository the chart pins.
	Upstream = "ghcr.io/agentgateway/agentgateway@" + Digest
	// MirrorVar names the repository that holds a copy of [Digest] (no tag or digest). CI's go-test
	// job sets it; the owner comes from the workflow, so it is not spelled here.
	MirrorVar = "KOINON_AGENTGATEWAY_MIRROR"
)

// Ref is the image reference to start: [Digest] in the repository [MirrorVar] names, or [Upstream]
// when it is unset. Both name the same digest, so the choice changes where the image comes from,
// never what runs.
func Ref() string {
	if repo := os.Getenv(MirrorVar); repo != "" {
		return repo + "@" + Digest
	}
	return Upstream
}
