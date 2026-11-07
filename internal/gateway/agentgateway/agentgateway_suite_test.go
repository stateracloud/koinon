// SPDX-License-Identifier: Apache-2.0

package agentgateway_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestAgentgateway(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "gateway/agentgateway")
}
