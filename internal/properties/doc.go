// SPDX-License-Identifier: Apache-2.0

// Package properties holds the property tests of issue 26: random orderings of events, replies,
// answers and restarts against the real queries on Postgres. It has no production code.
// Generators are seeded (internal/testing/propseed); design adapted from hydro-project/infinity
// (Apache-2.0).
package properties
