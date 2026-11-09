// SPDX-License-Identifier: Apache-2.0

package helperpkg

import "testing"

// The suite entry point runs RunSpecs through a helper.
func TestSuiteViaHelper(t *testing.T) { runSuite(t) }

// A plain test that reaches RequireT through a helper in a non-test file.
func TestHelmViaFile(t *testing.T) { needHelm(t) }

func TestPlainOK(t *testing.T) {}
