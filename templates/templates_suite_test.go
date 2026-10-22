// SPDX-License-Identifier: Apache-2.0

package templates_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestTemplates(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Templates Suite")
}
