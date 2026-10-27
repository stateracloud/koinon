// SPDX-License-Identifier: Apache-2.0

package gitea_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestGitea(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gitea Provider Suite")
}
