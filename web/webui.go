// SPDX-License-Identifier: Apache-2.0

//go:build webui

// Package web embeds the built web UI. Build the UI first (pnpm -C web
// build, or make build) and then the API with -tags webui; without the tag
// Dist returns nil and koinon-api serves no UI.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built SPA rooted at dist/.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the embed pattern guarantees dist exists
	}
	return sub
}
