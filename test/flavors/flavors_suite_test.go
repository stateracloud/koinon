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
// The suite skips when Docker is unavailable or a flavor image is missing; build the images
// with `make flavor-opencode` (or set KOINON_FLAVOR_BUILD=1 to build them here). Override an
// image with KOINON_FLAVOR_IMAGE_<NAME>, for example KOINON_FLAVOR_IMAGE_OPENCODE.
package flavors_test

import (
	"context"
	"os/exec"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestFlavors(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "flavor contract")
}

var dockerErr error

var _ = BeforeSuite(func() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		dockerErr = &dockerUnavailableError{out: string(out), err: err}
	}
})

type dockerUnavailableError struct {
	out string
	err error
}

func (e *dockerUnavailableError) Error() string { return e.err.Error() + ": " + e.out }
