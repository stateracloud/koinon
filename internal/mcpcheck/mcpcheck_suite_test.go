// SPDX-License-Identifier: Apache-2.0

package mcpcheck_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestMCPCheck(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "mcpcheck")
}
