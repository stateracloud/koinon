// SPDX-License-Identifier: Apache-2.0

package modelcheck_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/modelcheck"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

var _ = Describe("Compare", func() {
	It("accepts the line, with surrounding whitespace or one pair of quotes", func() {
		for _, reply := range []string{modelcheck.Line, "  " + modelcheck.Line + "\n", "\"" + modelcheck.Line + "\"", "`" + modelcheck.Line + "`"} {
			ok, diff := modelcheck.Compare(modelcheck.Line, reply)
			Expect(ok).To(BeTrue(), reply)
			Expect(diff).To(Equal([]modelcheck.Segment{{Op: modelcheck.Equal, Text: modelcheck.Line}}))
		}
	})

	It("reports any other change, character for character", func() {
		for _, reply := range []string{
			"Maria Schneider, Hauptstrasse 12, 10115 Berlin, namespace payments-prod-eu.",
			"maria Schneider, Hauptstrasse 12, 10115 Berlin, namespace payments-prod-eu",
			"Sure! " + modelcheck.Line,
			"",
		} {
			ok, _ := modelcheck.Compare(modelcheck.Line, reply)
			Expect(ok).To(BeFalse(), reply)
		}
	})

	It("shows masked words as deleted and inserted, and the rest as equal", func() {
		_, diff := modelcheck.Compare("name Maria Schneider, namespace dev", "name [NAME], namespace dev")
		Expect(diff).To(Equal([]modelcheck.Segment{
			{Op: modelcheck.Equal, Text: "name "},
			{Op: modelcheck.Deleted, Text: "Maria Schneider"},
			{Op: modelcheck.Inserted, Text: "[NAME]"},
			{Op: modelcheck.Equal, Text: ", namespace dev"},
		}))
	})

	It("rebuilds both texts from the segments", func() {
		sent, got := "a b c d", "a x c y d"
		_, diff := modelcheck.Compare(sent, got)
		var left, right string
		for _, s := range diff {
			if s.Op != modelcheck.Inserted {
				left += s.Text
			}
			if s.Op != modelcheck.Deleted {
				right += s.Text
			}
		}
		Expect(left).To(Equal(sent))
		Expect(right).To(Equal(got))
	})
})

type key string

func (k key) Key(context.Context, pgtype.UUID) (string, error) { return string(k), nil }

var _ = Describe("GatewayChat", func() {
	It("posts one chat completion with the org's key and returns the reply", func(ctx SpecContext) {
		var auth, path string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth, path = r.Header.Get("Authorization"), r.URL.Path
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`))
		}))
		DeferCleanup(srv.Close)
		got, err := modelcheck.GatewayChat{URL: srv.URL + "/", Keys: key("koinon_test_not-a-real-token")}.Complete(ctx, pgtype.UUID{}, "m", modelformat.OpenAI, "hi")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("hello"))
		Expect(path).To(Equal("/v1/chat/completions"))
		Expect(auth).To(Equal("Bearer koinon_test_not-a-real-token"))
	})

	// The check only compares text, so it sends no sampling parameters: newer models reject them
	// (Anthropic: "temperature is deprecated for this model").
	DescribeTable("sends only model, messages, max_tokens and stream",
		func(ctx SpecContext, format modelformat.Format, path string) {
			var gotPath string
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				_ = json.NewDecoder(r.Body).Decode(&body)
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"x"}}],"content":[{"type":"text","text":"x"}]}`))
			}))
			DeferCleanup(srv.Close)
			_, err := modelcheck.GatewayChat{URL: srv.URL, Keys: key("k")}.Complete(ctx, pgtype.UUID{}, "m", format, "hi")
			Expect(err).NotTo(HaveOccurred())
			Expect(gotPath).To(Equal(path))
			Expect(body).NotTo(HaveKey("temperature"))
			Expect(body).NotTo(HaveKey("top_p"))
			Expect(body).NotTo(HaveKey("top_k"))
			Expect(body).To(HaveLen(4))
			Expect(body).To(HaveKey("model"))
			Expect(body).To(HaveKey("messages"))
			Expect(body).To(HaveKey("max_tokens"))
			Expect(body).To(HaveKey("stream"))
		},
		Entry("openai: chat completions", modelformat.OpenAI, "/v1/chat/completions"),
		Entry("anthropic: Messages", modelformat.Anthropic, "/v1/messages"),
	)

	It("reports a gateway refusal", func(ctx SpecContext) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":"unknown model"}`, http.StatusNotFound)
		}))
		DeferCleanup(srv.Close)
		_, err := modelcheck.GatewayChat{URL: srv.URL, Keys: key("k")}.Complete(ctx, pgtype.UUID{}, "m", modelformat.OpenAI, "hi")
		Expect(err).To(MatchError(ContainSubstring("404")))
	})

	// The check posts to the route of the model's format, as a run does (ADR 0069), against the
	// fake model server's own routes, which answer in each API's shape.
	Describe("against the fake model server", func() {
		masked := strings.NewReplacer("Maria Schneider", "[NAME]", "Hauptstrasse 12, 10115 Berlin", "[ADDRESS]").Replace(modelcheck.Line)
		var fake *fakemodel.Server
		var srv *httptest.Server
		BeforeEach(func() {
			var err error
			fake, err = fakemodel.New(
				fakemodel.Script{Model: "faithful", Turns: []fakemodel.Turn{{Text: modelcheck.Line}}},
				fakemodel.Script{Model: "masking", Turns: []fakemodel.Turn{{Text: masked}}},
			)
			Expect(err).NotTo(HaveOccurred())
			srv = httptest.NewServer(fake)
			DeferCleanup(srv.Close)
		})
		gw := func() modelcheck.GatewayChat {
			return modelcheck.GatewayChat{URL: srv.URL, Keys: key("koinon_test_not-a-real-token")}
		}
		paths := func() []string {
			var out []string
			for _, r := range fake.Requests() {
				out = append(out, r.Path)
			}
			return out
		}

		DescribeTable("returns the reply, and posts to the route of the format",
			func(ctx SpecContext, format modelformat.Format, path string) {
				got, err := gw().Complete(ctx, pgtype.UUID{}, "faithful", format, modelcheck.Prompt)
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(Equal(modelcheck.Line))
				Expect(paths()).To(Equal([]string{path}))
			},
			Entry("openai: chat completions", modelformat.OpenAI, "/v1/chat/completions"),
			Entry("anthropic: Messages", modelformat.Anthropic, "/v1/messages"),
			Entry("no format: chat completions", modelformat.Format(""), "/v1/chat/completions"),
		)

		DescribeTable("shows what a masking filter changed, in either format",
			func(ctx SpecContext, format modelformat.Format) {
				got, err := gw().Complete(ctx, pgtype.UUID{}, "masking", format, modelcheck.Prompt)
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(Equal(masked))
				ok, diff := modelcheck.Compare(modelcheck.Line, got)
				Expect(ok).To(BeFalse())
				Expect(diff).To(Equal([]modelcheck.Segment{
					{Op: modelcheck.Deleted, Text: "Maria Schneider, Hauptstrasse 12"},
					{Op: modelcheck.Inserted, Text: "[NAME]"},
					{Op: modelcheck.Equal, Text: ", "},
					{Op: modelcheck.Deleted, Text: "10115 Berlin"},
					{Op: modelcheck.Inserted, Text: "[ADDRESS]"},
					{Op: modelcheck.Equal, Text: ", namespace payments-prod-eu"},
				}))
			},
			Entry("openai", modelformat.OpenAI),
			Entry("anthropic", modelformat.Anthropic),
		)

		It("reports a Messages reply with no text", func(ctx SpecContext) {
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"content":[]}`)) }))
			DeferCleanup(bad.Close)
			_, err := modelcheck.GatewayChat{URL: bad.URL, Keys: key("k")}.Complete(ctx, pgtype.UUID{}, "m", modelformat.Anthropic, "hi")
			Expect(err).To(MatchError(ContainSubstring("no text")))
		})
	})
})
