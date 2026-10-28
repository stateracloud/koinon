// SPDX-License-Identifier: Apache-2.0

package modelcheck_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestModelCheck(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Model check Suite")
}
