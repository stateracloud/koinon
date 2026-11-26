// SPDX-License-Identifier: Apache-2.0

package approvalroute_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway/approvalroute"
)

// countingDB fails every query and counts them: the specs below must be refused before any row is read.
type countingDB struct{ n atomic.Int32 }

var errNoDB = errors.New("no database in this spec")

func (d *countingDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	d.n.Add(1)
	return pgconn.CommandTag{}, errNoDB
}

func (d *countingDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	d.n.Add(1)
	return nil, errNoDB
}

func (d *countingDB) QueryRow(context.Context, string, ...any) pgx.Row {
	d.n.Add(1)
	return errRow{}
}

type errRow struct{}

func (errRow) Scan(...any) error { return errNoDB }

func noApprove(context.Context, store.Org, store.Agent, store.McpServer, store.McpTool, json.RawMessage, map[string]any) (string, error) {
	Fail("approve must not be called")
	return "", nil
}

var _ = Describe("the approval route of koinon-gateway (#1301)", func() {
	const token = "koinon_test_not-a-real-token"
	open := func(next http.Handler) http.Handler { return next }
	bearerOnly := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	It("is never built without a database, an approve function or an authentication wrapper", func() {
		db := &countingDB{}
		_, err := approvalroute.NewServer(approvalroute.ServerConfig{Approve: noApprove, Auth: open})
		Expect(err).To(HaveOccurred())
		_, err = approvalroute.NewServer(approvalroute.ServerConfig{DB: db, Auth: open})
		Expect(err).To(HaveOccurred())
		_, err = approvalroute.NewServer(approvalroute.ServerConfig{DB: db, Approve: noApprove})
		Expect(err).To(MatchError(ContainSubstring("never served open")))
		_, err = approvalroute.NewClient(approvalroute.ClientConfig{Token: func() (string, error) { return token, nil }})
		Expect(err).To(HaveOccurred())
		_, err = approvalroute.NewClient(approvalroute.ClientConfig{URL: "http://koinon-api"})
		Expect(err).To(HaveOccurred())
	})

	Describe("a request", func() {
		var (
			db     *countingDB
			client func(tok string) *approvalroute.Client
			org    store.Org
			agent  store.Agent
			srv    store.McpServer
			tool   store.McpTool
		)
		BeforeEach(func() {
			db = &countingDB{}
			route, err := approvalroute.NewServer(approvalroute.ServerConfig{DB: db, Approve: noApprove, Auth: bearerOnly})
			Expect(err).NotTo(HaveOccurred())
			passedOn := false
			api := httptest.NewServer(route.Mount()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				passedOn = true
				w.WriteHeader(http.StatusTeapot)
			})))
			DeferCleanup(api.Close)
			DeferCleanup(func() { Expect(passedOn).To(BeFalse(), "the route's paths never reach the next handler") })
			client = func(tok string) *approvalroute.Client {
				c, err := approvalroute.NewClient(approvalroute.ClientConfig{URL: api.URL, Token: func() (string, error) { return tok, nil }})
				Expect(err).NotTo(HaveOccurred())
				return c
			}
			org = store.Org{Slug: "acme"}
			Expect(agent.ID.Scan("6f1c1d3e-6a53-4c39-9a52-6b0c6d1f9a01")).To(Succeed())
			Expect(srv.ID.Scan("0b7e2b1e-2f0c-4f39-8d3e-0e2c4c8a7b02")).To(Succeed())
			tool = store.McpTool{Name: "post_message"}
		})

		approve := func(c *approvalroute.Client, args string, risk map[string]any) error {
			_, err := c.Approve(context.Background(), org, agent, srv, tool, json.RawMessage(args), risk)
			return err
		}

		It("without the token is refused before any row is read", func() {
			Expect(approve(client("koinon_test_not-the-token"), `{}`, nil)).To(HaveOccurred())
			Expect(db.n.Load()).To(BeZero())
		})

		It("with a missing or malformed name is refused before any row is read", func() {
			org.Slug = ""
			Expect(approve(client(token), `{}`, nil)).To(MatchError(ContainSubstring("invalid_argument")))
			org.Slug = strings.Repeat("a", 257)
			Expect(approve(client(token), `{}`, nil)).To(MatchError(ContainSubstring("invalid_argument")))
			org.Slug = "acme"
			agent.ID.Valid = false
			Expect(approve(client(token), `{}`, nil)).To(MatchError(ContainSubstring("invalid_argument")))
			Expect(db.n.Load()).To(BeZero())
		})

		It("with a risk that is too large is refused before any row is read", func() {
			Expect(approve(client(token), `{}`, map[string]any{"match": strings.Repeat("x", approvalroute.MaxRiskBytes)})).To(MatchError(ContainSubstring("invalid_argument")))
			Expect(db.n.Load()).To(BeZero())
		})

		It("fails closed when koinon-api cannot read its rows, and says nothing about them", func() {
			err := approve(client(token), `{"text":"hello"}`, map[string]any{"reasons": []string{"both_legs"}})
			Expect(err).To(MatchError(ContainSubstring("internal")))
			Expect(err.Error()).NotTo(ContainSubstring(errNoDB.Error()))
			Expect(db.n.Load()).To(BeNumerically(">", 0))
		})
	})
})
