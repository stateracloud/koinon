// SPDX-License-Identifier: Apache-2.0

package mcpguard

// SetFeedPolled sets a function the guard runs before each step of its wait for agentd's feed.
func SetFeedPolled(g *Guard, f func()) { g.feedPolled = f }
