// SPDX-License-Identifier: Apache-2.0

package router_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestRouter(t *testing.T) {
	RegisterFailHandler(Fail)
	// Eventually returns at the first success; its deadline only tells a hang from a slow machine.
	// Gomega's default of 1 s was a guess about the machine, and a loaded external gate missed it (koinon-private#612).
	SetDefaultEventuallyTimeout(hangBound)
	RunSpecs(t, "Trace Router Suite")
}
