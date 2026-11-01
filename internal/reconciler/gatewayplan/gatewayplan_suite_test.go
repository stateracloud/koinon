// SPDX-License-Identifier: Apache-2.0

package gatewayplan_test

import (
	"flag"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// update rewrites golden files: go test ./internal/reconciler/gatewayplan -args -update
var update = flag.Bool("update", false, "rewrite golden files")

func TestGatewayPlan(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gateway Plan Suite")
}
