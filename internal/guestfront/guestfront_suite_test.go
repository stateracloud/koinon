// SPDX-License-Identifier: Apache-2.0

package guestfront_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestGuestFront(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Guest Front Suite")
}
