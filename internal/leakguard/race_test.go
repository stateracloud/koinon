// SPDX-License-Identifier: Apache-2.0

//go:build race

package leakguard_test

// raceFactor scales the wall-clock backstop the specs give a benign call. The race detector slows
// the decoders by roughly an order of magnitude; the work budget (bytes, not time) is unchanged.
const raceFactor = 10
