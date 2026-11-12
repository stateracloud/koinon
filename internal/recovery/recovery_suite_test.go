// SPDX-License-Identifier: Apache-2.0

package recovery_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestRecovery(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Recovery Suite")
}
