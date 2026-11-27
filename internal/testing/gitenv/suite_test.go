// SPDX-License-Identifier: Apache-2.0

package gitenv_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestGitenv(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Git test environment")
}
