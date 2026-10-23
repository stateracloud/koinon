// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"flag"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// update rewrites golden files: go test ./internal/gateway/standalone -args -update
var update = flag.Bool("update", false, "rewrite golden files")

func TestStandalone(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Standalone Gateway Suite")
}
