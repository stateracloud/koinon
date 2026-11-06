// SPDX-License-Identifier: Apache-2.0

package fakemodel_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

var _ = Describe("reasoning tokens", func() {
	post := func(srv *fakemodel.Server, path, body string) string {
		ts := httptest.NewServer(srv)
		DeferCleanup(ts.Close)
		resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body)) //nolint:noctx // a local test server
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		return string(b)
	}
	newServer := func(u fakemodel.Usage) *fakemodel.Server {
		srv, err := fakemodel.New(fakemodel.Script{Model: "m", Turns: []fakemodel.Turn{{Text: "ok", Usage: u}}})
		Expect(err).NotTo(HaveOccurred())
		return srv
	}

	It("reports a split in chat completions and Messages usage only when set", func() {
		think := fakemodel.Usage{InputTokens: 10, OutputTokens: 8, ReasoningTokens: 5}
		Expect(post(newServer(think), "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)).
			To(ContainSubstring(`"completion_tokens_details":{"reasoning_tokens":5}`))
		Expect(post(newServer(think), "/v1/messages", `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)).
			To(ContainSubstring(`"output_tokens_details":{"thinking_tokens":5}`))

		plain := fakemodel.Usage{InputTokens: 10, OutputTokens: 8}
		Expect(post(newServer(plain), "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)).
			NotTo(ContainSubstring("completion_tokens_details"))
		Expect(post(newServer(plain), "/v1/messages", `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)).
			NotTo(ContainSubstring("output_tokens_details"))
	})
})
