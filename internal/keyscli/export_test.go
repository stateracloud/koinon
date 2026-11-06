// SPDX-License-Identifier: Apache-2.0

package keyscli

// HasLostRule reports whether keys forget knows what to do with a row of c.
func HasLostRule(c Column) bool {
	_, ok := lostRules[c]
	return ok
}
