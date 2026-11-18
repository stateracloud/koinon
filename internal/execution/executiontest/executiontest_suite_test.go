// SPDX-License-Identifier: Apache-2.0

package executiontest_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution/executiontest"
)

func TestExecutionTest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Execution Contract Suite")
}

var _ = executiontest.ComputeContract("Fake", func() executiontest.Harness {
	f := executiontest.NewFake()
	return executiontest.Harness{
		Compute: f, Workspace: "org-acme", Image: "img", Exec: []string{"cat"},
		Uploaded: func(sandbox, destDir, name string) ([]byte, bool) {
			return f.File("org-acme", sandbox, destDir+"/"+name)
		},
	}
})
