// SPDX-License-Identifier: Apache-2.0

package mcpgateway_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestMCPGateway(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "MCP Gateway Suite")
}
