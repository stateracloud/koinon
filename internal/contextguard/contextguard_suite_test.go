// SPDX-License-Identifier: Apache-2.0

package contextguard_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestContextguard(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Contextguard Suite")
}
