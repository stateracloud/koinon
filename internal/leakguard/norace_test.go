// SPDX-License-Identifier: Apache-2.0

//go:build !race

package leakguard_test

// raceFactor is 1 without the race detector: the specs keep the product's own 5 s backstop.
const raceFactor = 1
