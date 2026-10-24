// SPDX-License-Identifier: Apache-2.0

package secretstore_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestSecretstore(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Secretstore Suite")
}
