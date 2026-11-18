// SPDX-License-Identifier: Apache-2.0

package guestlaunch_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestGuestlaunch(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Guest launcher Suite")
}
