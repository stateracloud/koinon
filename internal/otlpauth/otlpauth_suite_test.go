// SPDX-License-Identifier: Apache-2.0

package otlpauth_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestOTLPAuth(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "OTLP receiver authentication")
}
