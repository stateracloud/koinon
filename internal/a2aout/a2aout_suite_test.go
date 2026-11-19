// SPDX-License-Identifier: Apache-2.0

package a2aout_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestA2AOut(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "A2A Outbound Suite")
}
