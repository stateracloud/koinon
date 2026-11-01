// SPDX-License-Identifier: Apache-2.0

package turnpolicy_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestTurnPolicy(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "agentd turn policy")
}
