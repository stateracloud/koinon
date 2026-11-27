// SPDX-License-Identifier: Apache-2.0

package gitenvcheck_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestGitenvcheck(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Git isolation in tests (static)")
}
