// SPDX-License-Identifier: Apache-2.0

package route_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestRoute(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gateway Route Suite")
}
