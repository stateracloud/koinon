// SPDX-License-Identifier: Apache-2.0

package loopguard_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestLoopguard(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "agentd loop guard")
}
