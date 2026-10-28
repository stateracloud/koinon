// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const fakeToken = "koinon_test_not-a-real-token"

func TestFakeCICost(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "koinon-fakecicost")
}

// bearerTransport adds the Authorization header, as agentgateway would.
type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

var _ = Describe("koinon-fakecicost", func() {
	// 2026-10-28 is a Wednesday; yesterday is 2026-10-27.
	now := time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC)
	var (
		s  *server
		ts *httptest.Server
	)

	BeforeEach(func() {
		s = &server{token: fakeToken, now: func() time.Time { return now }, recs: &requestLog{}}
		ts = httptest.NewServer(s.handler())
		DeferCleanup(ts.Close)
	})

	get := func(token, query string) (int, []byte) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/v1/costs?"+query, nil)
		Expect(err).NotTo(HaveOccurred())
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		return resp.StatusCode, b
	}
	rest := func(query string) costsResponse {
		code, body := get(fakeToken, query)
		Expect(code).To(Equal(http.StatusOK), string(body))
		var out costsResponse
		Expect(json.Unmarshal(body, &out)).To(Succeed())
		return out
	}

	Describe("auth", func() {
		It("refuses a missing or wrong token with 401 and records the attempt", func() {
			code, _ := get("", "from=2026-10-20&to=2026-10-21")
			Expect(code).To(Equal(http.StatusUnauthorized))
			code, _ = get("wrong", "from=2026-10-20&to=2026-10-21")
			Expect(code).To(Equal(http.StatusUnauthorized))
			recs := s.recs.snapshot()
			Expect(recs).To(HaveLen(2))
			Expect(recs[0].Authorized).To(BeFalse())
		})
		It("refuses the MCP endpoint without the token", func() {
			resp, err := http.Post(ts.URL+"/mcp", "application/json", http.NoBody)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = resp.Body.Close() }()
			Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
		})
		It("serves /healthz and /_fake/requests without the token", func() {
			for _, p := range []string{"/healthz", "/_fake/requests"} {
				resp, err := http.Get(ts.URL + p)
				Expect(err).NotTo(HaveOccurred())
				_ = resp.Body.Close()
				Expect(resp.StatusCode).To(Equal(http.StatusOK))
			}
		})
	})

	Describe("ranges", func() {
		It("returns one row per day without group_by", func() {
			out := rest("from=2026-10-20&to=2026-10-26")
			Expect(out.Rows).To(HaveLen(7))
			Expect(out.Rows[0].Date).To(Equal("2026-10-20"))
			Expect(out.TotalUSD).To(BeNumerically(">", 0))
		})
		It("covers the last 60 days including today", func() {
			out := rest("from=2026-08-30&to=2026-10-28")
			Expect(out.Rows).To(HaveLen(60))
		})
		It("rejects bad input with 400", func() {
			for _, q := range []string{
				"", "from=2026-10-20", "from=nope&to=2026-10-21", "from=2026-10-22&to=2026-10-21",
				"from=2026-08-29&to=2026-10-21", "from=2026-10-20&to=2026-10-29",
				"from=2026-10-20&to=2026-10-21&group_by=team",
			} {
				code, _ := get(fakeToken, q)
				Expect(code).To(Equal(http.StatusBadRequest), q)
			}
		})
	})

	Describe("grouping", func() {
		It("groups by repo, workflow and runner and the groups add up to the total", func() {
			total := rest("from=2026-10-20&to=2026-10-21")
			for group, want := range map[string]int{"repo": 8, "workflow": 8, "runner": 8} {
				out := rest("from=2026-10-20&to=2026-10-21&group_by=" + group)
				Expect(out.GroupBy).To(Equal(group))
				Expect(out.Rows).To(HaveLen(want))
				Expect(out.Rows[0].Key).NotTo(BeEmpty())
				Expect(out.TotalUSD).To(BeNumerically("~", total.TotalUSD, 0.05))
			}
		})
	})

	Describe("determinism", func() {
		It("returns identical bytes for the same date", func() {
			_, a := get(fakeToken, "from=2026-10-01&to=2026-10-20&group_by=repo")
			_, b := get(fakeToken, "from=2026-10-01&to=2026-10-20&group_by=repo")
			Expect(a).To(Equal(b))
		})
		It("gives a date the same data whatever time of day it is asked", func() {
			a := rest("from=2026-10-10&to=2026-10-12&group_by=repo")
			now = now.Add(5 * time.Hour)
			b := rest("from=2026-10-10&to=2026-10-12&group_by=repo")
			Expect(b).To(Equal(a))
		})
	})

	Describe("the planted anomaly", func() {
		It("doubles the repo's cost yesterday against the same weekday a week earlier", func() {
			cost := func(day string) map[string]float64 {
				m := map[string]float64{}
				for _, r := range rest("from=" + day + "&to=" + day + "&group_by=repo").Rows {
					m[r.Key] = r.CostUSD
				}
				return m
			}
			yesterday, lastWeek := cost("2026-10-27"), cost("2026-10-20")
			Expect(yesterday[anomalyRepo] / lastWeek[anomalyRepo]).To(BeNumerically("~", 2.0, 0.3))
			for _, repo := range repos {
				if repo != anomalyRepo {
					Expect(yesterday[repo]/lastWeek[repo]).To(BeNumerically("~", 1.0, 0.3), repo)
				}
			}
		})
		It("sits in the test workflow", func() {
			yesterday := rest("from=2026-10-27&to=2026-10-27&group_by=workflow")
			before := rest("from=2026-10-20&to=2026-10-20&group_by=workflow")
			for i, r := range yesterday.Rows {
				if r.Key == anomalyWorkflow {
					Expect(r.CostUSD).To(BeNumerically(">", 1.5*before.Rows[i].CostUSD))
				}
			}
		})
		It("moves with the clock", func() {
			now = now.AddDate(0, 0, 1)
			a := rest("from=2026-10-28&to=2026-10-28&group_by=repo")
			b := rest("from=2026-10-21&to=2026-10-21&group_by=repo")
			for i, r := range a.Rows {
				if r.Key == anomalyRepo {
					Expect(r.CostUSD / b.Rows[i].CostUSD).To(BeNumerically("~", 2.0, 0.3))
				}
			}
		})
	})

	Describe("the MCP tool", func() {
		connect := func(token string) (*mcp.ClientSession, error) {
			c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
			return c.Connect(context.Background(), &mcp.StreamableClientTransport{
				Endpoint:   ts.URL + "/mcp",
				HTTPClient: &http.Client{Transport: bearerTransport{token: token}},
			}, nil)
		}
		It("cannot connect without the token", func() {
			_, err := connect("")
			Expect(err).To(HaveOccurred())
		})
		It("lists ci_costs and answers like the REST endpoint", func() {
			sess, err := connect(fakeToken)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = sess.Close() }()
			tools, err := sess.ListTools(context.Background(), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(tools.Tools).To(HaveLen(1))
			Expect(tools.Tools[0].Name).To(Equal("ci_costs"))

			res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "ci_costs",
				Arguments: map[string]any{"from": "2026-10-26", "to": "2026-10-27", "group_by": "repo"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.IsError).To(BeFalse())
			text, ok := res.Content[0].(*mcp.TextContent)
			Expect(ok).To(BeTrue())
			var out costsResponse
			Expect(json.Unmarshal([]byte(text.Text), &out)).To(Succeed())
			Expect(out).To(Equal(rest("from=2026-10-26&to=2026-10-27&group_by=repo")))
		})
		It("returns a tool error for a bad range", func() {
			sess, err := connect(fakeToken)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = sess.Close() }()
			res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "ci_costs", Arguments: map[string]any{"from": "2026-10-27", "to": "2026-10-26"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.IsError).To(BeTrue())
		})
	})
})
