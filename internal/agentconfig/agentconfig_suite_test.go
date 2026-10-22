// SPDX-License-Identifier: Apache-2.0

package agentconfig_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestAgentConfig(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "agentconfig")
}
