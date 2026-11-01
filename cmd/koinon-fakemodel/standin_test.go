// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestStandin(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "koinon-fakemodel standin")
}

var _ = Describe("standin mode", func() {
	It("answers every method and path with an empty 418 and the marker header", func() {
		srv := httptest.NewServer(standinHandler())
		DeferCleanup(srv.Close)
		for _, c := range []struct{ method, path string }{
			{"POST", "/v1/messages"}, {"GET", "/v1/models"}, {"POST", "/config_dump"}, {"DELETE", "/"},
		} {
			req, err := http.NewRequest(c.method, srv.URL+c.path, strings.NewReader(`{"model":"x"}`))
			Expect(err).NotTo(HaveOccurred())
			res, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			body, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			Expect(res.StatusCode).To(Equal(http.StatusTeapot), c.method+" "+c.path)
			Expect(res.Header.Get("X-Koinon-Standin")).To(Equal("1"), c.method+" "+c.path)
			Expect(body).To(BeEmpty(), c.method+" "+c.path)
		}
	})

	It("serves over TLS with a given certificate", func() {
		srv := httptest.NewTLSServer(standinHandler())
		DeferCleanup(srv.Close)
		res, err := srv.Client().Get(srv.URL + "/gitea/api/v1/user")
		Expect(err).NotTo(HaveOccurred())
		_ = res.Body.Close()
		Expect(res.StatusCode).To(Equal(http.StatusTeapot))
	})
})
