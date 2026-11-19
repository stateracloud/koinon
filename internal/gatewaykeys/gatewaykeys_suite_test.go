// SPDX-License-Identifier: Apache-2.0

package gatewaykeys_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestGatewayKeys(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gateway Keys Suite")
}
