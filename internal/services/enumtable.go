// SPDX-License-Identifier: Apache-2.0

package services

// enumTable maps the values of a database enum (a store type sqlc generates) to what the API shows
// for them. Reads use the map syntax. The totality spec in enumtable_test.go checks each table
// against the generated All<Type>Values list, so a new enum value in a migration fails there until
// its table has a row.
type enumTable[S ~string, P comparable] map[S]P

// audit compares the table with every value of its database enum. It returns the values without a
// row, the values that map to the zero value of P (the UNSPECIFIED proto value, or an empty
// string), and the rows that name a value the database does not have.
func (t enumTable[S, P]) audit(all []S) (missing, zero, extra []string) {
	var z P
	seen := make(map[S]bool, len(all))
	for _, v := range all {
		seen[v] = true
		p, ok := t[v]
		switch {
		case !ok:
			missing = append(missing, string(v))
		case p == z:
			zero = append(zero, string(v))
		}
	}
	for k := range t {
		if !seen[k] {
			extra = append(extra, string(k))
		}
	}
	return missing, zero, extra
}
