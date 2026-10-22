// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// uiCSP keeps the SPA to its own origin. Inline styles are allowed because
// Radix positions popovers with style attributes.
const uiCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

// spaHandler serves the built web UI: files that exist as they are, and
// index.html for every other path so the client-side router can take it.
// Vite fingerprints everything under assets/, so those are cached for a
// year; index.html is never cached, so a new build takes effect at once.
func spaHandler(ui fs.FS) http.Handler {
	files := http.FileServerFS(ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Registered as "/" rather than "GET /": a method pattern would
		// conflict with connect's method-less service prefixes.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")

		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "index.html" {
			if st, err := fs.Stat(ui, name); err == nil && !st.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					h.Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			// A missing file with an extension is a real 404, not a route.
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
		}
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Security-Policy", uiCSP)
		http.ServeFileFS(w, r, ui, "index.html")
	})
}
