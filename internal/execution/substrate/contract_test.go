// SPDX-License-Identifier: Apache-2.0

package substrate_test

import (
	"github.com/stateracloud/koinon/internal/execution/executiontest"
)

// The Substrate driver, on the fake Substrate, passes the contract every driver must pass,
// including the optional suspend and snapshot specs: it implements both capabilities.
var _ = executiontest.ComputeContract("Substrate", func() executiontest.Harness {
	d, cl := newDriver()
	return executiontest.Harness{
		Compute: d, Workspace: ws, Image: "img", Exec: []string{"cat"},
		Uploaded: func(sandbox, destDir, name string) ([]byte, bool) {
			return cl.File(ws, sandbox, destDir+"/"+name)
		},
	}
})
