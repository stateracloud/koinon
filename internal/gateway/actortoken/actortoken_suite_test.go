// SPDX-License-Identifier: Apache-2.0

package actortoken_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestActorToken(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Actor Token Suite")
}
