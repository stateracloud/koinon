// SPDX-License-Identifier: Apache-2.0

// Package containerhost gives tests the address to dial a published container port on.
//
// testcontainers reports "localhost", which resolves to ::1 first. Docker runtimes that publish a
// port on both families (OrbStack) accept a connection to [::1] and then reset it when the
// process in the container listens on IPv4 only, so the first request fails with ECONNRESET for
// as long as the test retries (old issue 290). These helpers return the IPv4 loopback instead.
package containerhost

import (
	"context"
	"strings"

	"github.com/testcontainers/testcontainers-go"
)

// Host is the container's host, with "localhost" replaced by 127.0.0.1.
func Host(ctx context.Context, ctr testcontainers.Container) (string, error) {
	h, err := ctr.Host(ctx)
	if h == "localhost" {
		h = "127.0.0.1"
	}
	return h, err
}

// Endpoint is the URL of the container's published port ("4000/tcp") for proto, on Host.
func Endpoint(ctx context.Context, ctr *testcontainers.DockerContainer, port, proto string) (string, error) {
	u, err := ctr.PortEndpoint(ctx, port, proto)
	return strings.Replace(u, "//localhost:", "//127.0.0.1:", 1), err
}
