// SPDX-License-Identifier: Apache-2.0

package a2aedge_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestA2AEdge(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "A2A Edge Suite")
}
