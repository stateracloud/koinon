// SPDX-License-Identifier: Apache-2.0

package sandboxres_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestSandboxres(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "sandboxres Suite")
}
