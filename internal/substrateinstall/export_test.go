// SPDX-License-Identifier: Apache-2.0

package substrateinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// ExportPin is exportPin for the tests: the export step of BuildSetup (a fresh bare clone, the commit check,
// git archive and extractTar), so a test sees the tree the installer sees.
func (i *Installer) ExportPin(ctx context.Context, tmp, dst string) error {
	return i.exportPin(ctx, tmp, dst)
}

// PinGVisorForTest makes the pinned digest of each architecture the SHA-256 of body for the test, so a fake
// fetcher needs no 60 MB tarball. It returns the key of each architecture.
func PinGVisorForTest(t interface{ Cleanup(func()) }, body []byte) {
	saved := append([]gvisorArch(nil), gvisorArches...)
	sum := sha256.Sum256(body)
	for k := range gvisorArches {
		gvisorArches[k].sha256 = hex.EncodeToString(sum[:])
	}
	t.Cleanup(func() { copy(gvisorArches, saved) })
}

// GVisorKey is the object key of an architecture (GOARCH) under the pin in force.
func GVisorKey(goArch string) string {
	for _, a := range gvisorArches {
		if a.goArch == goArch {
			return gvisorKey(a)
		}
	}
	return ""
}

// GVisorSource is the source URL of an architecture (GOARCH) below the mirror.
func GVisorSource(mirror, goArch string) string {
	for _, a := range gvisorArches {
		if a.goArch == goArch {
			return gvisorSourceURL(mirror, a)
		}
	}
	return ""
}
