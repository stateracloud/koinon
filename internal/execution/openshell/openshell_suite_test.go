// SPDX-License-Identifier: Apache-2.0

package openshell_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
)

func TestOpenShell(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "OpenShell Backend Suite")
}

const ws = "org-acme"

// newBackend starts a fake gateway with workspace ws and returns a backend connected to it.
func newBackend() (*openshell.Backend, *fakegateway.Gateway) {
	gw := fakegateway.New()
	conn, err := gw.Start()
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(gw.Stop)
	DeferCleanup(conn.Close)
	b := openshell.New(conn)
	_, err = b.EnsureWorkspace(context.Background(), ws, nil)
	Expect(err).NotTo(HaveOccurred())
	return b, gw
}
