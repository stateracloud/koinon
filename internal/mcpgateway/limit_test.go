// SPDX-License-Identifier: Apache-2.0

package mcpgateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/mcpgateway"
)

var _ = Describe("the bound on an upstream reply", func() {
	const limit = 1024
	read := func(contentType, body string, chunked bool) (string, int, error) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", contentType)
			if !chunked {
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			}
			_, _ = io.WriteString(w, body)
		}))
		defer srv.Close()
		n := 0
		c := &http.Client{Transport: mcpgateway.LimitTransport(http.DefaultTransport, limit, func() { n++ })}
		resp, err := c.Get(srv.URL)
		if err != nil {
			return "", n, err
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		return string(b), n, err
	}
	It("passes a reply of exactly the limit", func() {
		b, n, err := read("application/json", strings.Repeat("x", limit), false)
		Expect(err).NotTo(HaveOccurred())
		Expect(b).To(HaveLen(limit))
		Expect(n).To(BeZero())
	})
	It("refuses a reply whose Content-Length is over the limit, before it reads the body", func() {
		_, n, err := read("application/json", strings.Repeat("x", limit+1), false)
		Expect(err).To(MatchError(mcpgateway.ErrResponseTooLarge))
		Expect(n).To(Equal(1))
	})
	It("cuts a reply with no Content-Length at the limit", func() {
		// 64 KiB over a handler with no Content-Length is sent chunked.
		b, n, err := read("application/json", strings.Repeat("x", 64<<10), true)
		Expect(err).To(MatchError(mcpgateway.ErrResponseTooLarge))
		Expect(len(b)).To(BeNumerically("<=", limit))
		Expect(n).To(Equal(1))
	})
	It("leaves a stream to the per-event bound of the SDK", func() {
		b, n, err := read("text/event-stream", strings.Repeat("data: x\n\n", 1000), false)
		Expect(err).NotTo(HaveOccurred())
		Expect(len(b)).To(BeNumerically(">", limit))
		Expect(n).To(BeZero())
	})
})
