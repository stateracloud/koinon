// SPDX-License-Identifier: Apache-2.0

package runwatch_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestRunwatch(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "runwatch")
}
