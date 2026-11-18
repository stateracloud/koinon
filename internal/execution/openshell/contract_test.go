// SPDX-License-Identifier: Apache-2.0

package openshell_test

import (
	"time"

	"github.com/stateracloud/koinon/internal/execution/executiontest"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
)

// The OpenShell driver, on the fake gateway, passes the contract every driver must pass.
var _ = executiontest.ComputeContract("OpenShell", func() executiontest.Harness {
	b, gw := newBackend()
	gw.Handle("cat", fakegateway.Cat)
	ex := &extracted{}
	gw.HandleUpload(ex.handler(time.Microsecond))
	return executiontest.Harness{
		Compute: b, Workspace: ws, Image: "img", Exec: []string{"cat"},
		Uploaded: func(_, destDir, name string) ([]byte, bool) {
			ex.mu.Lock()
			defer ex.mu.Unlock()
			body, ok := ex.files[name]
			return body, ok && ex.dest == destDir
		},
	}
})
