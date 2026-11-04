// SPDX-License-Identifier: Apache-2.0

package compat_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCompat(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Persisted-state compatibility Suite")
}
