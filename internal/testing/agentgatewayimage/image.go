// SPDX-License-Identifier: Apache-2.0

// Package agentgatewayimage names the agentgateway image the Go tests start (#238). It is the image
// the chart pins (gateway.standalone.image in charts/koinon/values.yaml), by digest, so a test runs
// what an install runs; hack/agentgateway-pin.sh --check fails when [Digest] differs from the pin.
//
// CI pulls the copy in GHCR that .github/workflows/mirror-agentgateway-test-image.yml keeps, so an
// outage of the upstream registry no longer turns every Go job red. The go-test job logs in to GHCR
// and sets [MirrorVar]; everywhere else (a laptop, the lab) the tests pull the same digest from the
// upstream registry, which needs no credentials. docs/dev/ci.md, "The agentgateway test image".
package agentgatewayimage

import "os"

const (
	// Digest is the chart's gateway.standalone.image digest.
	Digest = "sha256:60216aafb03d37f94e952ff3d51d2f630a65b10ecaebdc0a864882f92610c45b"
	// Mirror is the GHCR copy of [Digest] (private until v0.3; the go-test job logs in).
	Mirror = "ghcr.io/stateracloud/agentgateway@" + Digest
	// Upstream is [Digest] in agentgateway's own registry.
	Upstream = "cr.agentgateway.dev/agentgateway@" + Digest
	// MirrorVar set to "true" makes [Ref] return [Mirror]. CI's go-test job sets it.
	MirrorVar = "KOINON_AGENTGATEWAY_MIRROR"
)

// Ref is the image reference to start: [Mirror] when [MirrorVar] is "true", else [Upstream]. Both
// name the same digest, so the choice changes where the image comes from, never what runs.
func Ref() string {
	if os.Getenv(MirrorVar) == "true" {
		return Mirror
	}
	return Upstream
}
