// SPDX-License-Identifier: Apache-2.0

package codehost_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCodehost(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Codehost Suite")
}
