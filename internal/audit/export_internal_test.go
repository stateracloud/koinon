// SPDX-License-Identifier: Apache-2.0

package audit

// SetExportPage makes Export read n rows per query and returns the function that restores the page
// size, so a spec can export a short chain across several pages.
func SetExportPage(n int32) (restore func()) {
	old := exportPage
	exportPage = n
	return func() { exportPage = old }
}
