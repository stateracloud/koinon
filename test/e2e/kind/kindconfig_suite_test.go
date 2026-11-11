// SPDX-License-Identifier: Apache-2.0

package kind_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestKindConfig(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Kind cluster config")
}
