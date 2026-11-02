// SPDX-License-Identifier: Apache-2.0

package fakemodel_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

func faulty(f fakemodel.Faults) fakemodel.Script {
	return fakemodel.Script{Model: "faulty", Faults: f, Turns: []fakemodel.Turn{{Text: "one two three four five six"}}}
}

var _ = Describe("Faults", func() {
	openAI := func(ctx context.Context, url string, stream bool) *http.Response {
		return post(ctx, url+"/v1/chat/completions", map[string]any{
			"model": "faulty", "stream": stream, "messages": []any{userMsg("hi")},
		})
	}

	It("answers 503 for the first N requests, then succeeds", func(ctx SpecContext) {
		_, ts := start(faulty(fakemodel.Faults{Unavailable: 1}))

		resp := openAI(ctx, ts.URL, false)
		Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
		var body struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		decode(resp, &body)
		Expect(body.Error.Type).To(Equal("server_error"))

		resp = openAI(ctx, ts.URL, false)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(resp.Body.Close()).To(Succeed())
	})

	It("refuses the first N requests with the scripted client error, then succeeds", func(ctx SpecContext) {
		_, ts := start(faulty(fakemodel.Faults{Refuse: 1, RefuseStatus: http.StatusUnauthorized}))
		resp := openAI(ctx, ts.URL, false)
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
		Expect(resp.Header.Get("x-should-retry")).To(BeEmpty())
		Expect(resp.Body.Close()).To(Succeed())
		resp = openAI(ctx, ts.URL, false)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(resp.Body.Close()).To(Succeed())
	})

	It("refuses with 400 when no status is scripted", func(ctx SpecContext) {
		_, ts := start(faulty(fakemodel.Faults{Refuse: 1}))
		resp := openAI(ctx, ts.URL, false)
		Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		Expect(resp.Body.Close()).To(Succeed())
	})

	It("marks scripted faults with x-should-retry: false", func(ctx SpecContext) {
		_, ts := start(faulty(fakemodel.Faults{RateLimit: 1, RetryAfter: 7, Refuse: 1, DoNotRetry: true}))
		resp := openAI(ctx, ts.URL, false)
		Expect(resp.StatusCode).To(Equal(http.StatusTooManyRequests))
		Expect(resp.Header.Get("Retry-After")).To(Equal("7"))
		Expect(resp.Header.Get("x-should-retry")).To(Equal("false"))
		Expect(resp.Body.Close()).To(Succeed())
		resp = openAI(ctx, ts.URL, false)
		Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		Expect(resp.Header.Get("x-should-retry")).To(Equal("false"))
		Expect(resp.Body.Close()).To(Succeed())
	})

	It("answers 503 on the Anthropic API with an overloaded_error body", func(ctx SpecContext) {
		_, ts := start(faulty(fakemodel.Faults{Unavailable: 1}))
		resp := post(ctx, ts.URL+"/v1/messages", map[string]any{"model": "faulty", "max_tokens": 1, "messages": []any{anUser("hi")}})
		Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
		var body struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		decode(resp, &body)
		Expect(body.Error.Type).To(Equal("overloaded_error"))
	})

	It("resets the outage counter when a script is registered again", func(ctx SpecContext) {
		srv, ts := start(faulty(fakemodel.Faults{Unavailable: 1}))
		Expect(openAI(ctx, ts.URL, false).Body.Close()).To(Succeed())
		Expect(srv.Register(faulty(fakemodel.Faults{Unavailable: 1}))).To(Succeed())
		resp := openAI(ctx, ts.URL, false)
		Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
		Expect(resp.Body.Close()).To(Succeed())
	})

	It("delays the answer so client timeouts fire", func(ctx SpecContext) {
		_, ts := start(faulty(fakemodel.Faults{Delay: 2 * time.Second}))
		short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		req, err := http.NewRequestWithContext(short, http.MethodPost, ts.URL+"/v1/chat/completions",
			jsonReader(map[string]any{"model": "faulty", "messages": []any{userMsg("hi")}}))
		Expect(err).NotTo(HaveOccurred())
		began := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		Expect(err).To(MatchError(context.DeadlineExceeded))
		Expect(time.Since(began)).To(BeNumerically("<", time.Second))
	})

	It("answers after the delay when the client waits", func(ctx SpecContext) {
		_, ts := start(faulty(fakemodel.Faults{Delay: 150 * time.Millisecond}))
		began := time.Now()
		resp := openAI(ctx, ts.URL, false)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(resp.Body.Close()).To(Succeed())
		Expect(time.Since(began)).To(BeNumerically(">=", 150*time.Millisecond))
	})

	It("truncates an OpenAI stream before [DONE] and drops the connection", func(ctx SpecContext) {
		_, ts := start(faulty(fakemodel.Faults{Truncate: true}))
		resp := openAI(ctx, ts.URL, true)
		DeferCleanup(resp.Body.Close)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		events, err := readSSE(resp.Body)
		Expect(err).To(MatchError(io.ErrUnexpectedEOF))
		Expect(events).NotTo(BeEmpty())
		for _, e := range events {
			Expect(e.data).NotTo(Equal("[DONE]"))
		}
	})

	It("truncates an Anthropic stream before message_stop", func(ctx SpecContext) {
		_, ts := start(faulty(fakemodel.Faults{Truncate: true}))
		resp := post(ctx, ts.URL+"/v1/messages", map[string]any{
			"model": "faulty", "max_tokens": 1, "stream": true, "messages": []any{anUser("hi")},
		})
		DeferCleanup(resp.Body.Close)
		events, err := readSSE(resp.Body)
		Expect(err).To(HaveOccurred())
		Expect(events).NotTo(BeEmpty())
		Expect(events[0].name).To(Equal("message_start"))
		for _, e := range events {
			Expect(e.name).NotTo(Equal("message_stop"))
		}
	})

	It("truncates a non-streaming body", func(ctx SpecContext) {
		_, ts := start(faulty(fakemodel.Faults{Truncate: true}))
		resp := openAI(ctx, ts.URL, false)
		DeferCleanup(resp.Body.Close)
		_, err := io.ReadAll(resp.Body)
		Expect(errors.Is(err, io.ErrUnexpectedEOF)).To(BeTrue(), "got %v", err)
	})
})

func jsonReader(v any) io.Reader {
	GinkgoHelper()
	b, err := json.Marshal(v)
	Expect(err).NotTo(HaveOccurred())
	return bytes.NewReader(b)
}
