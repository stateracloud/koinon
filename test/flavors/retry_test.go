// SPDX-License-Identifier: Apache-2.0

package flavors_test

import (
	"bytes"
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/acp"
	"github.com/stateracloud/koinon/internal/agentd"
	koinflavor "github.com/stateracloud/koinon/internal/flavor"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

// One layer retries a failed model request, with Koinon's policy (ADR 0056 amendment 2026-10-31,
// point 3; old issue 341). OpenCode 2.2.0 would retry 10 times on its own schedule, and koinon-agentd
// used to repeat the turn 3 more times on top: about 44 gateway requests for one failed step.
// Koinon's koinon.retry plugin decides each of OpenCode's retries from koinon-opencode's switches,
// and koinon-agentd repeats no turn for this flavor, so the requests counted here are all a failed
// step costs.
var _ = Describe("koinon-opencode model retries", func() {
	// primary are the agent's own model requests: they carry the instructions, the session title
	// requests (OpenCode's one-shot calls, not the agent's step) do not.
	primary := func(gw *gateway) []fakemodel.Request {
		var out []fakemodel.Request
		for _, r := range gw.model.Requests() {
			if bytes.Contains(r.Body, []byte(instructionsMarker)) {
				out = append(out, r)
			}
		}
		return out
	}

	// failing prompts once against a model that answers every request with faults, and returns
	// the agent's requests once the turn has ended, with how long the turn took.
	failing := func(ctx SpecContext, faults fakemodel.Faults, extra ...string) ([]fakemodel.Request, time.Duration) {
		GinkgoHelper()
		script := contractScript(opencode)
		script.Faults = faults
		gw := newGateway(script)
		sb := newOpencodeSandbox(ctx, instructionsMarker+"\n")
		h, session := opencodeHarness(ctx, sb, gw, extra...)
		start := time.Now()
		stop, err := h.conn.Prompt(ctx, session, helloPrompt)
		took := time.Since(start)
		GinkgoWriter.Printf("prompt ended after %s: stop %q, err %v\n", took, stop, err)
		Expect(err).To(HaveOccurred(), "the turn fails once the retries end")
		// No try is left over to run after the turn ended.
		n := len(primary(gw))
		Consistently(func() int { return len(primary(gw)) }).WithTimeout(3 * time.Second).Should(Equal(n))
		return primary(gw), took
	}

	gaps := func(rs []fakemodel.Request) []time.Duration {
		var out []time.Duration
		for i := 1; i < len(rs); i++ {
			out = append(out, rs[i].At.Sub(rs[i-1].At))
		}
		return out
	}

	// slack is what a gap may run over its wait: the request leaves OpenCode, crosses Docker's
	// network and reaches the fake model.
	const slack = 750 * time.Millisecond

	It("tries a 503 Koinon's three times, waiting 1s then 2s with jitter, by default", func(ctx SpecContext) {
		rs, _ := failing(ctx, fakemodel.Faults{Unavailable: 1000})
		Expect(rs).To(HaveLen(3), "3 tries, not OpenCode's 11")
		g := gaps(rs)
		Expect(g[0]).To(BeNumerically(">=", 800*time.Millisecond))
		Expect(g[0]).To(BeNumerically("<=", 1200*time.Millisecond+slack))
		Expect(g[1]).To(BeNumerically(">=", 1600*time.Millisecond))
		Expect(g[1]).To(BeNumerically("<=", 2400*time.Millisecond+slack))
	}, SpecTimeout(3*time.Minute))

	It("follows the tries and backoff it is given", func(ctx SpecContext) {
		rs, _ := failing(ctx, fakemodel.Faults{Unavailable: 1000}, "--model-tries", "4", "--model-retry-backoff-ms", "200")
		Expect(rs).To(HaveLen(4))
		for i, g := range gaps(rs) {
			base := 200 * time.Millisecond << i
			Expect(g).To(BeNumerically(">=", base*8/10), "gap %d", i)
			Expect(g).To(BeNumerically("<=", base*12/10+slack), "gap %d", i)
		}
	}, SpecTimeout(3*time.Minute))

	It("does not retry with --model-tries 1", func(ctx SpecContext) {
		rs, _ := failing(ctx, fakemodel.Faults{Unavailable: 1000}, "--model-tries", "1")
		Expect(rs).To(HaveLen(1))
	}, SpecTimeout(3*time.Minute))

	It("waits out a Retry-After up to the cap", func(ctx SpecContext) {
		rs, _ := failing(ctx, fakemodel.Faults{RateLimit: 1000, RetryAfter: 2}, "--model-tries", "2", "--model-max-retry-after-ms", "5000")
		Expect(rs).To(HaveLen(2))
		Expect(gaps(rs)[0]).To(BeNumerically(">=", 2*time.Second), "the provider's Retry-After, not the 1s backoff")
		Expect(gaps(rs)[0]).To(BeNumerically("<=", 2*time.Second+slack))
	}, SpecTimeout(3*time.Minute))

	It("ends the retries at once on a Retry-After beyond the cap", func(ctx SpecContext) {
		rs, took := failing(ctx, fakemodel.Faults{RateLimit: 1000, RetryAfter: 120})
		Expect(rs).To(HaveLen(1), "a 120s Retry-After is beyond the 60s Koinon waits")
		Expect(took).To(BeNumerically("<", 30*time.Second), "the turn is not held for the Retry-After")
	}, SpecTimeout(3*time.Minute))

	// Koinon never retries a refusal (koinon.retry's own rule, before OpenCode's retryable table):
	// a client error other than 408, 409 and 429, or anything the gateway marks x-should-retry: false.
	DescribeTable("does not retry a refusal",
		func(ctx SpecContext, faults fakemodel.Faults) {
			rs, took := failing(ctx, faults)
			Expect(rs).To(HaveLen(1))
			Expect(took).To(BeNumerically("<", 30*time.Second))
		},
		Entry("400, a malformed request", fakemodel.Faults{Refuse: 1000, RefuseStatus: 400}, SpecTimeout(3*time.Minute)),
		Entry("401, a bad key the gateway cannot mark", fakemodel.Faults{Refuse: 1000, RefuseStatus: 401}, SpecTimeout(3*time.Minute)),
		Entry("403, no access", fakemodel.Faults{Refuse: 1000, RefuseStatus: 403}, SpecTimeout(3*time.Minute)),
		Entry("a 429 the gateway marks x-should-retry: false", fakemodel.Faults{RateLimit: 1000, RetryAfter: 1, DoNotRetry: true}, SpecTimeout(3*time.Minute)),
	)

	It("ends at once on the standalone gateway's budget refusal, a 429 with a Retry-After of hours", func(ctx SpecContext) {
		// old issue 342 cannot mark this one in standalone mode: Koinon's Retry-After cap makes it final, and
		// koinon-agentd then fails the turn as throttled (internal/agentd failure_class_test.go).
		rs, took := failing(ctx, fakemodel.Faults{RateLimit: 1000, RetryAfter: 11812})
		Expect(rs).To(HaveLen(1))
		Expect(took).To(BeNumerically("<", 30*time.Second), "the turn is not held for the Retry-After")
	}, SpecTimeout(3*time.Minute))

	It("answers the turn once a retry gets through", func(ctx SpecContext) {
		// The first three requests get a 429, whether the agent's or the session title's: the agent's
		// step has three tries, so it gets through whichever way they fall.
		script := contractScript(opencode)
		script.Faults = fakemodel.Faults{RateLimit: 3, RetryAfter: 1}
		gw := newGateway(script)
		sb := newOpencodeSandbox(ctx, instructionsMarker+"\n")
		h, session := opencodeHarness(ctx, sb, gw)
		stop, err := h.conn.Prompt(ctx, session, helloPrompt)
		Expect(err).NotTo(HaveOccurred())
		Expect(stop).To(Equal(acp.StopEndTurn))
		updates, _ := h.client.take()
		Expect(acp.Text(updates, "agent_message_chunk")).To(ContainSubstring(helloReply))
		Expect(len(gw.model.Requests())).To(BeNumerically(">=", 4), "the three 429s and the answered request")
		Expect(len(primary(gw))).To(BeNumerically("<=", 3))
	}, SpecTimeout(3*time.Minute))
})

var _ = Describe("koinon-opencode retry switches", Ordered, func() {
	var sb *sandbox

	BeforeAll(func(ctx SpecContext) {
		sb = newOpencodeSandbox(ctx, instructionsMarker+"\n")
	})

	// entryEnv runs a copy of the entry script whose last line prints the environment instead of
	// starting OpenCode.
	entryEnv := func(ctx context.Context, extra ...string) string {
		args := append([]string{
			"sh", "-c",
			`sed 's|^exec /usr/local/bin/opencode acp.*|exec /usr/bin/env|' /usr/local/bin/koinon-opencode > /tmp/entry && exec sh /tmp/entry "$@"`,
			"sh", "--model-base-url", "http://gw.test/v1", "--model", "m-1", "--mcp-url", "http://gw.test/mcp",
		}, extra...)
		return sb.run(ctx, args...)
	}

	It("is a flavor whose harness retries, in the flavor table", func() {
		d, ok := koinflavor.Lookup(opencode.storeFlavor)
		Expect(ok).To(BeTrue())
		Expect(d.RetriesModelRequests).To(BeTrue())
	})

	It("defaults to koinon-agentd's policy without the switches", func(ctx SpecContext) {
		env := entryEnv(ctx)
		Expect(env).To(ContainSubstring("KOINON_MODEL_TRIES=" + strconv.Itoa(agentd.DefaultModelRetries+1) + "\n"))
		Expect(env).To(ContainSubstring("KOINON_MODEL_RETRY_BACKOFF_MS=" + strconv.FormatInt(agentd.DefaultRetryBackoff.Milliseconds(), 10) + "\n"))
		Expect(env).To(ContainSubstring("KOINON_MODEL_MAX_RETRY_AFTER_MS=" + strconv.FormatInt(agentd.DefaultMaxRetryAfter.Milliseconds(), 10) + "\n"))
	})

	It("passes the switches to the retry plugin", func(ctx SpecContext) {
		env := entryEnv(ctx, "--model-tries", "2", "--model-retry-backoff-ms", "250", "--model-max-retry-after-ms", "0")
		Expect(env).To(ContainSubstring("KOINON_MODEL_TRIES=2\n"))
		Expect(env).To(ContainSubstring("KOINON_MODEL_RETRY_BACKOFF_MS=250\n"))
		Expect(env).To(ContainSubstring("KOINON_MODEL_MAX_RETRY_AFTER_MS=0\n"))
		Expect(env).NotTo(ContainSubstring("OPENCODE_CONFIG_CONTENT"), "the plugin is in the read-only config, not an overlay")
	})

	It("loads the plugin from the read-only config, from a directory nothing else loads", func(ctx SpecContext) {
		Expect(sb.run(ctx, "stat", "-c", "%u %A", "/usr/local/lib/koinon/opencode/plugins/koinon-retry/index.js")).
			To(Equal("0 -rw-r--r--\n"))
		Expect(sb.run(ctx, "cat", "/etc/koinon/opencode/opencode.jsonc")).
			To(ContainSubstring(`"package": "/usr/local/lib/koinon/opencode/plugins/koinon-retry"`))
	})

	It("refuses a policy it cannot follow", func(ctx SpecContext) {
		base := opencode.command("http://gw.test/v1", "m-1", "http://gw.test/mcp")
		for _, extra := range [][]string{
			{"--model-tries", "0"},
			{"--model-tries", "12"},
			{"--model-tries", "three"},
			{"--model-tries", ""},
			{"--model-tries"},
			{"--model-retry-backoff-ms", "-1"},
			{"--model-retry-backoff-ms", "1.5"},
			{"--model-max-retry-after-ms", "1e3"},
			{"--model-max-retry-after-ms", strings.Repeat("9", 10)},
		} {
			_, stderr, code := sb.try(ctx, append(slices.Clone(base), extra...)...)
			Expect(code).To(Equal(64), "%v: %s", extra, stderr)
			Expect(stderr).To(ContainSubstring("koinon-opencode:"), "%v", extra)
		}
	})
})
