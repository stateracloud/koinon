// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing/fstest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("the web UI handler", func() {
	ui := fstest.MapFS{
		"index.html":       {Data: []byte("<!doctype html><title>Koinon</title>")},
		"favicon.svg":      {Data: []byte("<svg/>")},
		"assets/app-1.js":  {Data: []byte("console.log(1)")},
		"assets/app-1.css": {Data: []byte("body{}")},
	}
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		spaHandler(ui).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	It("serves index.html for client-side routes, uncached and with a CSP", func() {
		for _, p := range []string{"/", "/tickets/abc", "/login"} {
			rec := get(p)
			Expect(rec.Code).To(Equal(http.StatusOK), p)
			Expect(rec.Body.String()).To(ContainSubstring("<title>Koinon</title>"), p)
			Expect(rec.Header().Get("Cache-Control")).To(Equal("no-cache"))
			Expect(rec.Header().Get("Content-Security-Policy")).To(ContainSubstring("frame-ancestors 'none'"))
		}
	})

	It("serves fingerprinted assets with a long cache lifetime", func() {
		rec := get("/assets/app-1.js")
		Expect(rec.Code).To(Equal(http.StatusOK))
		Expect(rec.Body.String()).To(Equal("console.log(1)"))
		Expect(rec.Header().Get("Cache-Control")).To(ContainSubstring("immutable"))
		Expect(get("/favicon.svg").Header().Get("Cache-Control")).To(BeEmpty())
	})

	It("answers 404 for missing files rather than the app", func() {
		Expect(get("/assets/missing.js").Code).To(Equal(http.StatusNotFound))
		Expect(get("/robots.txt").Code).To(Equal(http.StatusNotFound))
	})
})
