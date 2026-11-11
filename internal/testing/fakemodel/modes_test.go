// SPDX-License-Identifier: Apache-2.0

package fakemodel_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

var _ = Describe("Failure modes", func() {
	openAI := func(ctx SpecContext, url string, stream bool) *http.Response {
		return post(ctx, url+"/v1/chat/completions", map[string]any{
			"model": "faulty", "stream": stream, "messages": []any{userMsg("hi")},
		})
	}
	anthropic := func(ctx SpecContext, url string, stream bool) *http.Response {
		return post(ctx, url+"/v1/messages", map[string]any{
			"model": "faulty", "max_tokens": 1, "stream": stream, "messages": []any{anUser("hi")},
		})
	}

	Describe("rateLimit", func() {
		It("answers 429 with Retry-After for the first N requests on the OpenAI API", func(ctx SpecContext) {
			_, ts := start(faulty(fakemodel.Faults{RateLimit: 2, RetryAfter: 7}))
			for range 2 {
				resp := openAI(ctx, ts.URL, false)
				Expect(resp.StatusCode).To(Equal(http.StatusTooManyRequests))
				Expect(resp.Header.Get("Retry-After")).To(Equal("7"))
				var body struct {
					Error struct{ Type, Code string } `json:"error"`
				}
				decode(resp, &body)
				Expect(body.Error.Code).To(Equal("rate_limit_exceeded"))
			}
			resp := openAI(ctx, ts.URL, false)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(resp.Body.Close()).To(Succeed())
		})

		It("defaults Retry-After to 1 second and uses rate_limit_error on the Anthropic API", func(ctx SpecContext) {
			_, ts := start(faulty(fakemodel.Faults{RateLimit: 1}))
			resp := anthropic(ctx, ts.URL, false)
			Expect(resp.StatusCode).To(Equal(http.StatusTooManyRequests))
			Expect(resp.Header.Get("Retry-After")).To(Equal("1"))
			var body struct {
				Error struct{ Type string } `json:"error"`
			}
			decode(resp, &body)
			Expect(body.Error.Type).To(Equal("rate_limit_error"))
		})

		It("is reset by registering the script again", func(ctx SpecContext) {
			srv, ts := start(faulty(fakemodel.Faults{RateLimit: 1}))
			Expect(openAI(ctx, ts.URL, false).Body.Close()).To(Succeed())
			Expect(srv.Register(faulty(fakemodel.Faults{RateLimit: 1}))).To(Succeed())
			resp := openAI(ctx, ts.URL, false)
			Expect(resp.StatusCode).To(Equal(http.StatusTooManyRequests))
			Expect(resp.Body.Close()).To(Succeed())
		})
	})

	Describe("contextLength", func() {
		It("answers 400 context_length_exceeded in the OpenAI error shape, every time", func(ctx SpecContext) {
			_, ts := start(faulty(fakemodel.Faults{ContextLength: true}))
			for range 2 {
				resp := openAI(ctx, ts.URL, true)
				Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
				var body struct {
					Error struct {
						Message string `json:"message"`
						Type    string `json:"type"`
						Param   string `json:"param"`
						Code    string `json:"code"`
					} `json:"error"`
				}
				decode(resp, &body)
				Expect(body.Error.Code).To(Equal("context_length_exceeded"))
				Expect(body.Error.Type).To(Equal("invalid_request_error"))
				Expect(body.Error.Param).To(Equal("messages"))
				Expect(body.Error.Message).To(ContainSubstring("maximum context length"))
			}
		})

		It("answers 400 prompt is too long on the Anthropic API", func(ctx SpecContext) {
			_, ts := start(faulty(fakemodel.Faults{ContextLength: true}))
			resp := anthropic(ctx, ts.URL, false)
			Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
			var body struct {
				Error struct{ Type, Message string } `json:"error"`
			}
			decode(resp, &body)
			Expect(body.Error.Type).To(Equal("invalid_request_error"))
			Expect(body.Error.Message).To(HavePrefix("prompt is too long"))
		})
	})

	Describe("overloadedMidStream", func() {
		It("ends an OpenAI stream with an error chunk and no [DONE]", func(ctx SpecContext) {
			_, ts := start(faulty(fakemodel.Faults{OverloadedMidStream: true}))
			resp := openAI(ctx, ts.URL, true)
			DeferCleanup(resp.Body.Close)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			events, err := readSSE(resp.Body)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(events)).To(BeNumerically(">", 1))
			Expect(events[len(events)-1].data).To(ContainSubstring(`"code":"overloaded"`))
			for _, e := range events {
				Expect(e.data).NotTo(Equal("[DONE]"))
			}
		})

		It("ends an Anthropic stream with an overloaded_error event and no message_stop", func(ctx SpecContext) {
			_, ts := start(faulty(fakemodel.Faults{OverloadedMidStream: true}))
			resp := anthropic(ctx, ts.URL, true)
			DeferCleanup(resp.Body.Close)
			events, err := readSSE(resp.Body)
			Expect(err).NotTo(HaveOccurred())
			Expect(events[0].name).To(Equal("message_start"))
			last := events[len(events)-1]
			Expect(last.name).To(Equal("error"))
			Expect(last.data).To(ContainSubstring("overloaded_error"))
			for _, e := range events {
				Expect(e.name).NotTo(Equal("message_stop"))
			}
		})

		It("answers a non-streaming request with the 503 overloaded error", func(ctx SpecContext) {
			_, ts := start(faulty(fakemodel.Faults{OverloadedMidStream: true}))
			resp := anthropic(ctx, ts.URL, false)
			Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
			Expect(resp.Body.Close()).To(Succeed())
		})
	})

	Describe("splitWrites", func() {
		It("delivers the same events as an unsplit stream, in many reads", func(ctx SpecContext) {
			_, plain := start(faulty(fakemodel.Faults{}))
			resp := anthropic(ctx, plain.URL, true)
			DeferCleanup(resp.Body.Close)
			want, err := readSSE(resp.Body)
			Expect(err).NotTo(HaveOccurred())

			_, ts := start(faulty(fakemodel.Faults{SplitWrites: true}))
			resp = anthropic(ctx, ts.URL, true)
			DeferCleanup(resp.Body.Close)
			reads, buf := 0, make([]byte, 4096)
			var raw []byte
			for {
				n, err := resp.Body.Read(buf)
				if n > 0 {
					reads++
					raw = append(raw, buf[:n]...)
				}
				if err != nil {
					break
				}
			}
			got, err := readSSE(bytes.NewReader(raw))
			Expect(err).NotTo(HaveOccurred())
			names := func(es []event) (out []string) {
				for _, e := range es {
					out = append(out, e.name)
				}
				return out
			}
			Expect(names(got)).To(Equal(names(want)))
			Expect(reads).To(BeNumerically(">", 5*len(want)), "each event arrives in pieces")
		})
	})

	Describe("stallMidStream", func() {
		It("sends about half of the events and holds the stream open until the client leaves", func(ctx SpecContext) {
			_, ts := start(faulty(fakemodel.Faults{StallMidStream: true}))
			cctx, cancel := context.WithCancel(ctx)
			defer cancel()
			resp := post(cctx, ts.URL+"/v1/messages", map[string]any{
				"model": "faulty", "max_tokens": 1, "stream": true, "messages": []any{anUser("hi")},
			})
			DeferCleanup(resp.Body.Close)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			done := make(chan error, 1)
			go func() {
				_, err := io.Copy(io.Discard, resp.Body)
				done <- err
			}()
			Consistently(done, 300*time.Millisecond).ShouldNot(Receive(), "the stream stays open")
			cancel()
			Eventually(done).Should(Receive(HaveOccurred()))
		})
	})

	Describe("lockstep", func() {
		It("holds each response until the test releases it", func(ctx SpecContext) {
			srv, ts := start(faulty(fakemodel.Faults{Lockstep: true}))
			done := make(chan int, 2)
			for range 2 {
				go func() {
					defer GinkgoRecover()
					resp := openAI(ctx, ts.URL, false)
					_ = resp.Body.Close()
					done <- resp.StatusCode
				}()
			}
			Eventually(func() int { return srv.Waiting("faulty") }).Should(Equal(2))
			Consistently(done, 150*time.Millisecond).ShouldNot(Receive())

			Expect(srv.Release("faulty", 1)).To(Succeed())
			Eventually(done).Should(Receive(Equal(http.StatusOK)))
			Consistently(done, 150*time.Millisecond).ShouldNot(Receive())
			Expect(srv.Waiting("faulty")).To(Equal(1))

			Expect(srv.Release("faulty", 1)).To(Succeed())
			Eventually(done).Should(Receive(Equal(http.StatusOK)))
			Eventually(func() int { return srv.Waiting("faulty") }).Should(Equal(0))
		})

		It("keeps a release made before the request arrives", func(ctx SpecContext) {
			srv, ts := start(faulty(fakemodel.Faults{Lockstep: true}))
			Expect(srv.Release("faulty", 1)).To(Succeed())
			resp := anthropic(ctx, ts.URL, true)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(resp.Body.Close()).To(Succeed())
		})

		It("stops waiting when the client gives up", func(ctx SpecContext) {
			srv, ts := start(faulty(fakemodel.Faults{Lockstep: true}))
			short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			defer cancel()
			req, err := http.NewRequestWithContext(short, http.MethodPost, ts.URL+"/v1/chat/completions",
				jsonReader(map[string]any{"model": "faulty", "messages": []any{userMsg("hi")}}))
			Expect(err).NotTo(HaveOccurred())
			resp, err := http.DefaultClient.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			Expect(err).To(HaveOccurred())
			Eventually(func() int { return srv.Waiting("faulty") }).Should(Equal(0))
		})

		It("keeps held requests releasable after the model is re-registered", func(ctx SpecContext) {
			srv, ts := start(faulty(fakemodel.Faults{Lockstep: true}))
			done := make(chan int, 1)
			go func() {
				defer GinkgoRecover()
				resp := openAI(ctx, ts.URL, false)
				_ = resp.Body.Close()
				done <- resp.StatusCode
			}()
			Eventually(func() int { return srv.Waiting("faulty") }).Should(Equal(1))
			Expect(srv.Register(faulty(fakemodel.Faults{Lockstep: true}))).To(Succeed())
			Expect(srv.Waiting("faulty")).To(Equal(1))
			Expect(srv.Release("faulty", 1)).To(Succeed())
			Eventually(done).Should(Receive(Equal(http.StatusOK)))
			Eventually(func() int { return srv.Waiting("faulty") }).Should(Equal(0))
		})

		It("rejects releasing an unknown model", func() {
			srv, _ := start(faulty(fakemodel.Faults{Lockstep: true}))
			Expect(srv.Release("nope", 1)).To(HaveOccurred())
		})
	})

	It("loads the modes from YAML and rejects negative values", func() {
		s, err := fakemodel.ParseScript([]byte("model: m\nfaults: {rateLimit: 2, retryAfter: 5, contextLength: true, overloadedMidStream: true, stallMidStream: true, splitWrites: true, lockstep: true}\nturns: [{text: hi}]\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(s.Faults).To(Equal(fakemodel.Faults{RateLimit: 2, RetryAfter: 5, ContextLength: true, OverloadedMidStream: true, StallMidStream: true, SplitWrites: true, Lockstep: true}))
		_, err = fakemodel.ParseScript([]byte("model: m\nfaults: {rateLimit: -1}\nturns: [{text: hi}]\n"))
		Expect(err).To(HaveOccurred())
	})
})
