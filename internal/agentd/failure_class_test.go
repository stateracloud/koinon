// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// modelCaller makes the fake harness call the fake model server the way a harness does (a
// streaming Anthropic request) and report a failure the way harnesses report it over ACP: a
// JSON-RPC error whose data carries the HTTP status, the Retry-After header and the provider's
// error. A request the model answers is a success.
func modelCaller(url string) func(context.Context, string) error {
	return func(ctx context.Context, _ string) error {
		body, _ := json.Marshal(map[string]any{
			"model": "faulty", "max_tokens": 64, "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "work"}},
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			var e struct {
				Error struct{ Type, Message string } `json:"error"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&e)
			return &sdk.RequestError{Code: -32603, Message: e.Error.Message, Data: map[string]any{
				"statusCode": resp.StatusCode, "errorType": e.Error.Type,
				"headers": map[string]any{"retry-after": resp.Header.Get("Retry-After")},
			}}
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		event := ""
		for sc.Scan() {
			switch line := sc.Text(); {
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:") && event == "error":
				return &sdk.RequestError{Code: -32603, Message: "stream error", Data: map[string]any{
					"event": json.RawMessage(strings.TrimSpace(strings.TrimPrefix(line, "data:"))),
				}}
			}
		}
		return sc.Err()
	}
}

var _ = Describe("Failure classes", func() {
	var srv *fakemodel.Server

	// model starts a fake model server whose only model, "faulty", misbehaves as faults say.
	model := func(e *env, faults fakemodel.Faults) {
		var err error
		srv, err = fakemodel.New(fakemodel.Script{
			Model: "faulty", Faults: faults, Turns: []fakemodel.Turn{{Text: "one two three four"}},
		})
		Expect(err).NotTo(HaveOccurred())
		ts := httptest.NewServer(srv)
		DeferCleanup(ts.Close)
		e.harness.modelFault = modelCaller(ts.URL)
	}
	retries := func(ctx context.Context, r store.Run) []map[string]any {
		var out []map[string]any
		for _, ev := range events(ctx, r) {
			if ev.Kind == "run.retry" {
				var p map[string]any
				Expect(json.Unmarshal(ev.Payload, &p)).To(Succeed())
				out = append(out, p)
			}
		}
		return out
	}
	// The specs below exercise agentd's own retry layer: the fake harness does not retry, so they
	// run as for a harness whose harness does not either.
	quick := func(c *agentd.Config) { c.RetryBackoff = 10 * time.Millisecond; agentd.OwnRetryLayer(c) }
	failedClass := func(ctx context.Context, r store.Run) store.RunFailureClass {
		return getRun(ctx, r).FailureClass.RunFailureClass
	}

	It("waits out a 429's Retry-After, then retries and succeeds with no failure class", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		model(e, fakemodel.Faults{RateLimit: 1, RetryAfter: 1})
		start := time.Now()
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", quick)
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(time.Since(start)).To(BeNumerically(">=", time.Second), "the Retry-After is honoured")
		Expect(srv.Requests()).To(HaveLen(2))
		got := retries(ctx, r)
		Expect(got).To(HaveLen(1))
		Expect(got[0]).To(HaveKeyWithValue("failureClass", "throttled"))
		Expect(got[0]).To(HaveKeyWithValue("delaySeconds", BeNumerically("==", 1)))
		Expect(getRun(ctx, r).FailureClass.Valid).To(BeFalse())
	})

	It("retries a 503 with backoff and succeeds", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		model(e, fakemodel.Faults{Unavailable: 2})
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", quick)
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
		got := retries(ctx, r)
		Expect(got).To(HaveLen(2))
		Expect(got[0]).To(HaveKeyWithValue("failureClass", "transient"))
		Expect(got[1]["delaySeconds"]).To(BeNumerically(">", got[0]["delaySeconds"]), "the backoff grows")
	})

	It("fails a context-length 400 as context_overflow without retrying", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		model(e, fakemodel.Faults{ContextLength: true})
		t := e.task(ctx, "hello")
		r := e.enqueue(ctx, t)
		e.pool("agentd-a", quick)
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(failedClass(ctx, r)).To(Equal(store.RunFailureClassContextOverflow))
		Expect(srv.Requests()).To(HaveLen(1), "no blind retry")
		Expect(retries(ctx, r)).To(BeEmpty())
		Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring("context window"))
		tevs := taskEvents(ctx, e, t)
		last := tevs[len(tevs)-1]
		Expect(last.Kind).To(Equal("run.failed"))
		var payload map[string]string
		Expect(json.Unmarshal(last.Payload, &payload)).To(Succeed())
		Expect(payload).To(HaveKeyWithValue("failureClass", "context_overflow"))
	})

	It("retries a mid-stream overload with backoff, then fails the run as transient", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		model(e, fakemodel.Faults{OverloadedMidStream: true})
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", quick, func(c *agentd.Config) { c.ModelRetries = 2 })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(failedClass(ctx, r)).To(Equal(store.RunFailureClassTransient))
		Expect(srv.Requests()).To(HaveLen(3), "the first request and two retries")
		Expect(retries(ctx, r)).To(HaveLen(2))
		Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring("gave up after 2 retries"))
	})

	It("does not wait out a Retry-After longer than it is willing to", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		model(e, fakemodel.Faults{RateLimit: 5, RetryAfter: 30})
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", quick, func(c *agentd.Config) { c.MaxRetryAfter = 5 * time.Second })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(failedClass(ctx, r)).To(Equal(store.RunFailureClassThrottled))
		Expect(srv.Requests()).To(HaveLen(1))
		Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring("asked to wait 30s"))
	})

	// One layer retries (ADR 0056 amendment 2026-10-31, point 3). OpenCode retries model requests
	// itself with the policy agentd hands it (test/flavors/retry_test.go checks that end to end), so
	// a retryable failure that reaches agentd has used every try: agentd does not repeat the turn,
	// and one failed step costs the harness's tries only.
	Describe("for a harness that retries model requests itself", func() {
		fastBackoff := func(c *agentd.Config) { c.RetryBackoff = 10 * time.Millisecond }

		It("does not repeat the turn after a 503, and fails the run as transient", func(ctx SpecContext) {
			e := newEnv(ctx, harness.OpenCode)
			model(e, fakemodel.Faults{Unavailable: 5})
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", fastBackoff)
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
			Expect(failedClass(ctx, r)).To(Equal(store.RunFailureClassTransient))
			Expect(srv.Requests()).To(HaveLen(1), "the fake harness's one request; agentd adds none")
			Expect(retries(ctx, r)).To(BeEmpty())
			Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring("the harness stopped retrying (Koinon allows it up to 3 tries)"))
		})

		It("does not wait out a 429 either, and still names a Retry-After beyond the cap", func(ctx SpecContext) {
			e := newEnv(ctx, harness.OpenCode)
			model(e, fakemodel.Faults{RateLimit: 5, RetryAfter: 120})
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			start := time.Now()
			e.pool("agentd-a", fastBackoff)
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
			Expect(time.Since(start)).To(BeNumerically("<", 15*time.Second))
			Expect(failedClass(ctx, r)).To(Equal(store.RunFailureClassThrottled))
			Expect(srv.Requests()).To(HaveLen(1))
			Expect(retries(ctx, r)).To(BeEmpty())
			Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring("asked to wait 2m0s, longer than the 1m0s Koinon waits"))
		})

		It("fails the standalone gateway's budget refusal, a 429 with a Retry-After of hours, as throttled", func(ctx SpecContext) {
			// The gateway cannot mark it x-should-retry: false in standalone mode (old issue 342).
			e := newEnv(ctx, harness.OpenCode)
			model(e, fakemodel.Faults{RateLimit: 5, RetryAfter: 11812})
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", fastBackoff)
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
			Expect(failedClass(ctx, r)).To(Equal(store.RunFailureClassThrottled))
			Expect(srv.Requests()).To(HaveLen(1))
			Expect(retries(ctx, r)).To(BeEmpty())
			Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring("asked to wait 3h16m52s, longer than the 1m0s Koinon waits"))
		})

		It("launches the harness with the pool's retry policy", func(ctx SpecContext) {
			e := newEnv(ctx, harness.OpenCode)
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", fastBackoff, func(c *agentd.Config) { c.ModelRetries = 4; c.MaxRetryAfter = 30 * time.Second })
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
			_, _, _, launches := e.harness.snapshot()
			Expect(launches).To(ContainElement(ContainElements(
				"--model-tries", "5", "--model-retry-backoff-ms", "10", "--model-max-retry-after-ms", "30000")))
		})
	})

	DescribeTable("classifies an error the harness answers a prompt with, and never retries what is not transient",
		func(ctx SpecContext, err error, want store.RunFailureClass) {
			e := newEnv(ctx, harness.OpenCode)
			calls := 0
			e.harness.modelFault = func(context.Context, string) error { calls++; return err }
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", quick, func(c *agentd.Config) { c.ModelRetries = 1 })
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
			Expect(failedClass(ctx, r)).To(Equal(want))
			retryable := want == store.RunFailureClassTransient || want == store.RunFailureClassThrottled
			Expect(calls).To(Equal(map[bool]int{true: 2, false: 1}[retryable]), fmt.Sprint(want))
		},
		Entry("an unknown error is fatal", &sdk.RequestError{Code: -32603, Message: "boom"}, store.RunFailureClassFatal),
		Entry("a policy denial", &sdk.RequestError{Code: -32603, Message: "denied", Data: map[string]any{"statusCode": 403}}, store.RunFailureClassPolicy),
		Entry("a spend limit", &sdk.RequestError{Code: -32603, Message: "spend limit reached for the org"}, store.RunFailureClassBudget),
		Entry("a 502 in the text", &sdk.RequestError{Code: -32603, Message: "upstream said 502 Bad Gateway"}, store.RunFailureClassTransient),
		Entry("a 429 with Retry-After in the text", &sdk.RequestError{Code: -32603, Message: "429 Too Many Requests, Retry-After: 0"}, store.RunFailureClassThrottled),
	)
})
