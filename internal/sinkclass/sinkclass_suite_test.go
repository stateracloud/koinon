// SPDX-License-Identifier: Apache-2.0

package sinkclass_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestSinkclass(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Sink class")
}
