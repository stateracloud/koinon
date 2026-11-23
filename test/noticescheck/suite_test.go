// SPDX-License-Identifier: Apache-2.0

package noticescheck_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestNoticescheck(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Flavor image notices (static)")
}
