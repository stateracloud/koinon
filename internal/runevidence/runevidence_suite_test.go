// SPDX-License-Identifier: Apache-2.0

package runevidence_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestRunEvidence(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Run Evidence Suite")
}
