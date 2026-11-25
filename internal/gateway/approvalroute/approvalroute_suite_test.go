// SPDX-License-Identifier: Apache-2.0

package approvalroute_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestApprovalRoute(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gateway Approval Route Suite")
}
