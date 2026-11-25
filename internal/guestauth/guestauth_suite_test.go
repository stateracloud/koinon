// SPDX-License-Identifier: Apache-2.0

package guestauth_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestGuestAuth(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Guest Auth Suite")
}
