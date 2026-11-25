// SPDX-License-Identifier: Apache-2.0

package credkind_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCredkind(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gateway Credential Kinds Suite")
}
