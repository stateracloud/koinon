// SPDX-License-Identifier: Apache-2.0

//go:build !webui

// Package web embeds the built web UI. Build the UI first (pnpm -C web
// build, or make build) and then the API with -tags webui; without the tag
// Dist returns nil and koinon-api serves no UI.
package web

import "io/fs"

// Dist returns nil: this binary was built without -tags webui.
func Dist() fs.FS { return nil }
