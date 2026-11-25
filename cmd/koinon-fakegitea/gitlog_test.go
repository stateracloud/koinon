// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/fakegitea"
)

var _ = Describe("the git request record (#1086 part C2)", func() {
	It("keeps each git request with its status and whether it carried a credential, never the credential", func() {
		f := fakegitea.NewUnstarted()
		DeferCleanup(f.Close)
		f.Owner = "koinon-demo"
		f.AddRepo("hello")
		g := &gitLog{}
		h := g.record(f.Handler)

		serve := func(user, pass string) int {
			r := httptest.NewRequest(http.MethodGet, "/koinon-demo/hello.git/info/refs?service=git-upload-pack", nil)
			if user != "" {
				r.SetBasicAuth(user, pass)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			return w.Code
		}
		Expect(serve("", "")).To(Equal(http.StatusUnauthorized))
		Expect(serve(fakegitea.DefaultLogin, fakegitea.DefaultToken)).To(Equal(http.StatusOK))
		// Not a git path: not kept.
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))

		w = httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/_fake/git", nil))
		Expect(w.Body.String()).NotTo(ContainSubstring(fakegitea.DefaultToken))
		var got []gitRequest
		Expect(json.Unmarshal(w.Body.Bytes(), &got)).To(Succeed())
		Expect(got).To(Equal([]gitRequest{
			{Method: http.MethodGet, Path: "/koinon-demo/hello.git/info/refs", Service: "git-upload-pack", Status: http.StatusUnauthorized},
			{Method: http.MethodGet, Path: "/koinon-demo/hello.git/info/refs", Service: "git-upload-pack", Status: http.StatusOK, Auth: true},
		}))
	})

	It("keeps at most maxGitRequests", func() {
		g := &gitLog{}
		h := g.record(http.NotFoundHandler())
		for range maxGitRequests + 5 {
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/o/r.git/info/refs", nil))
		}
		Expect(g.reqs).To(HaveLen(maxGitRequests))
	})
})
