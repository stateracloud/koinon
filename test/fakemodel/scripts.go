// SPDX-License-Identifier: Apache-2.0

// Package fakemodelscripts embeds the fake model server's example scripts, so the
// koinon-fakemodel image carries them without a kodata symlink (ko refuses symlinks that leave
// the kodata root).
package fakemodelscripts

import "embed"

// FS holds scripts/*.yaml.
//
//go:embed scripts/*.yaml
var FS embed.FS
