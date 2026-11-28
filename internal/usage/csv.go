// SPDX-License-Identifier: Apache-2.0

package usage

import (
	"bytes"
	"encoding/csv"
	"strconv"
	"strings"
)

// CSV writes a table: the header, the rows and a closing row. Every text cell is made safe for a
// spreadsheet. The rows are written as given, so an export holds the rows of the API call it
// matches.
func CSV(header []string, rows [][]string, total []string) (string, error) {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	if err := w.Write(header); err != nil {
		return "", err
	}
	for _, r := range rows {
		if err := w.Write(r); err != nil {
			return "", err
		}
	}
	if err := w.Write(total); err != nil {
		return "", err
	}
	w.Flush()
	return b.String(), w.Error()
}

// Text makes a text cell safe: a spreadsheet runs a cell that starts with =, +, -, @, tab or
// carriage return as a formula, so such a cell gets a leading apostrophe. Names of agents, models
// and providers are set by users.
func Text(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// Int writes a count.
func Int(n int64) string { return strconv.FormatInt(n, 10) }

// Share writes a cache read share with four fraction digits.
func Share(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }
