// SPDX-License-Identifier: Apache-2.0

package export_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestExport(t *testing.T) {
	RegisterFailHandler(Fail)
	// Eventually returns at the first success; its deadline only tells a hang from a slow machine (old issue 612).
	SetDefaultEventuallyTimeout(hangBound)
	RunSpecs(t, "Trace Export Suite")
}
