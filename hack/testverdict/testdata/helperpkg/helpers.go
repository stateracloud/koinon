// SPDX-License-Identifier: Apache-2.0

// Fixture for hack/testverdict: helpers in a non-test file.
package helperpkg

import "testing"

func needHelm(t *testing.T) { testenv.RequireT(t, testenv.Helm) }

func runSuite(t *testing.T) { RunSpecs(t, "helperpkg") }
