// SPDX-License-Identifier: Apache-2.0

//go:build race

package scrub_test

// raceSlowdown scales time budgets: the race detector makes code several times slower.
const raceSlowdown = 20
