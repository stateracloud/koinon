// SPDX-License-Identifier: Apache-2.0

package credkind

// UnlistedForTest makes a kind that is not in the list. Only specs can make one; production code cannot.
func UnlistedForTest(name string) Kind { return Kind{name} }
