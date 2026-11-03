// SPDX-License-Identifier: Apache-2.0

package installid_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestInstallID(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Install ID Suite")
}
