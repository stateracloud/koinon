// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestGatewaytest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "gateway/gatewaytest")
}
