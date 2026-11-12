// SPDX-License-Identifier: Apache-2.0

//go:build !testing

package agentd_test

import . "github.com/onsi/ginkgo/v2"

// This suite's time budgets (kopia repository opens in the checkpoint specs, seed work) assume
// kopia's fast test key derivation, which the testing build tag selects. Without it a repository
// open takes seconds of scrypt under load, and specs fail in ways that look like product bugs (#638).
// The suite refuses to run instead of flaking.
func requireTestingTag() {
	Fail("internal/agentd specs need the testing build tag: run hack/go-test.sh, or set GOFLAGS=-tags=testing")
}
