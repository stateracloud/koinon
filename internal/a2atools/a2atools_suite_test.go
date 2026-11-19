// SPDX-License-Identifier: Apache-2.0

package a2atools_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestA2ATools(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "A2A Built-in Target Suite")
}
