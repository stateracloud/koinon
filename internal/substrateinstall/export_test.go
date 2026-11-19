// SPDX-License-Identifier: Apache-2.0

package substrateinstall

import "context"

// ExportPin is exportPin for the tests: the export step of BuildSetup (a fresh bare clone, the commit check,
// git archive and extractTar), so a test sees the tree the installer sees.
func (i *Installer) ExportPin(ctx context.Context, tmp, dst string) error {
	return i.exportPin(ctx, tmp, dst)
}
