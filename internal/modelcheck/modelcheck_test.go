// SPDX-License-Identifier: Apache-2.0

package modelcheck_test

import (
	"context"
	"net/http"
	"net/http/httptest"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/modelcheck"
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
		got, err := modelcheck.GatewayChat{URL: srv.URL + "/", Keys: key("koinon_test_not-a-real-token")}.Complete(ctx, pgtype.UUID{}, "m", "hi")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("hello"))
		Expect(path).To(Equal("/v1/chat/completions"))
		Expect(auth).To(Equal("Bearer koinon_test_not-a-real-token"))
	})

	It("reports a gateway refusal", func(ctx SpecContext) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":"unknown model"}`, http.StatusNotFound)
		}))
		DeferCleanup(srv.Close)
		_, err := modelcheck.GatewayChat{URL: srv.URL, Keys: key("k")}.Complete(ctx, pgtype.UUID{}, "m", "hi")
		Expect(err).To(MatchError(ContainSubstring("404")))
	})
})
