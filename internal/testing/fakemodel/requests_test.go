// SPDX-License-Identifier: Apache-2.0

package fakemodel_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

var _ = Describe("GET /_fake/requests", func() {
	It("lists the path and model of every request, in order", func(ctx SpecContext) {
		srv, err := fakemodel.New(fakemodel.Script{Model: "m", Turns: []fakemodel.Turn{{Text: "hi"}}})
		Expect(err).NotTo(HaveOccurred())
		ts := httptest.NewServer(srv)
		DeferCleanup(ts.Close)

		resp := post(ctx, ts.URL+"/v1/messages", map[string]any{"model": "m", "max_tokens": 8, "messages": []any{anUser("hi")}})
		_ = resp.Body.Close()
		resp = post(ctx, ts.URL+"/v1/chat/completions", map[string]any{"model": "m", "messages": []any{userMsg("hi")}})
		_ = resp.Body.Close()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/_fake/requests", nil)
		Expect(err).NotTo(HaveOccurred())
		res, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = res.Body.Close() }()
		var got []map[string]string
		Expect(json.NewDecoder(res.Body).Decode(&got)).To(Succeed())
		Expect(got).To(Equal([]map[string]string{
			{"path": "/v1/messages", "model": "m"},
			{"path": "/v1/chat/completions", "model": "m"},
		}))
	})
})
