// SPDX-License-Identifier: Apache-2.0

// Fixture for hack/testverdict: a Ginkgo suite entry point beside a plain test, like test/chartsmoke.
// testdata is ignored by the go tool; testverdict only parses it.
package plainsuite

import "testing"

func TestChartsmoke(t *testing.T) { RunSpecs(t, "chartsmoke") }

func TestChartContract(t *testing.T) { testenv.RequireT(t, testenv.Helm) }
