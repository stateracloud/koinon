// SPDX-License-Identifier: Apache-2.0

package agentmodel_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestAgentModel(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Agent model Suite")
}
