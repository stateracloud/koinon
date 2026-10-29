// SPDX-License-Identifier: Apache-2.0

package thinking_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestThinking(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "thinking")
}
