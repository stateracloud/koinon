// SPDX-License-Identifier: Apache-2.0

package mcptoolpin_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestMCPToolPin(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "MCP tool pin")
}
