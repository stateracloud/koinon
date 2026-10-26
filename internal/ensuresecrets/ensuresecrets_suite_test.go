// SPDX-License-Identifier: Apache-2.0

package ensuresecrets_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestEnsureSecrets(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Ensure Secrets Suite")
}
