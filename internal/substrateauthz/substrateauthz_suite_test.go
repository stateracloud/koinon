// SPDX-License-Identifier: Apache-2.0

package substrateauthz_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestSubstrateAuthz(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Substrate Authz Suite")
}
