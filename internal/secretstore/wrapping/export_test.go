// SPDX-License-Identifier: Apache-2.0

package wrapping

import "github.com/stateracloud/koinon/internal/secretstore"

// SetAdapter makes New build each KMS key with build, until the returned restore runs. Tests use it
// to stand fakes in for the OpenBao and AWS KMS servers.
func SetAdapter(build func(Key) (secretstore.Wrapper, error)) (restore func()) {
	prev := adapter
	adapter = build
	return func() { adapter = prev }
}

// RealAdapter is the adapter New uses outside tests: the OpenBao and AWS KMS adapters.
var RealAdapter = adapter
