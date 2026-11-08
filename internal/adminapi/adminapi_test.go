// SPDX-License-Identifier: Apache-2.0

package adminapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	adminv1 "github.com/stateracloud/koinon/api/admin/v1"
	"github.com/stateracloud/koinon/internal/adminapi"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/modelcheck"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/services"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/tokens"
	"github.com/stateracloud/koinon/pricing"
)

type env struct {
	srv     *httptest.Server
	api     *adminapi.Server
	tokens  *tokens.Service
	catalog *catalog.Service
	mcp     *mcpservers.Service
	now     *atomic.Pointer[time.Time]
	org     pgtype.UUID
	user    pgtype.UUID
	token   string // an owner token
	router  routers.Router
	// chat answers the echo check in place of the gateway.
	chat *fakeChat
}

func newEnv(ctx context.Context) *env {
	GinkgoHelper()
	e := &env{now: &atomic.Pointer[time.Time]{}}
	clock := func() time.Time {
		if t := e.now.Load(); t != nil {
			return *t
		}
		return time.Now()
	}
	e.tokens = tokens.New(pool, clock)
	ring := secretstoretest.Keyring()
	e.catalog = catalog.New(pool, ring, nil)
	e.mcp = mcpservers.New(pool, ring, nil).WithURLPolicy(catalog.URLPolicy{Namespace: "koinon"})
	mux := http.NewServeMux()
	e.chat = &fakeChat{}
	e.api = adminapi.New(pool, e.catalog, e.tokens, slog.New(slog.NewTextHandler(GinkgoWriter, nil)), pricing.Embedded()).WithMCP(e.mcp).
		WithTriggers(services.New(pool, services.Options{Prices: pricing.Embedded()})).WithModelCheck(modelcheck.New(pool, e.chat, prices.NewResolver(pricing.Embedded())))
	e.api.Mount(mux)
	e.srv = httptest.NewServer(mux)
	DeferCleanup(e.srv.Close)
	e.org, e.user, _ = pgtest.NewOrg(ctx, pool)
	c, err := e.tokens.Create(ctx, e.org, audit.User(e.user), tokens.Creator{User: e.user, Role: store.OrgRoleOwner}, "bootstrap", store.OrgRoleOwner, 0)
	Expect(err).NotTo(HaveOccurred())
	e.token = c.Secret

	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(adminv1.OpenAPI)
	Expect(err).NotTo(HaveOccurred())
	doc.Servers = nil
	e.router, err = legacy.NewRouter(doc)
	Expect(err).NotTo(HaveOccurred())
	return e
}

// call sends a request and checks the response against the OpenAPI document.
func (e *env) call(ctx context.Context, token, method, path string, body any) (int, []byte) {
	GinkgoHelper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		Expect(err).NotTo(HaveOccurred())
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.srv.URL+"/admin/v1"+path, rd)
	Expect(err).NotTo(HaveOccurred())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)

	// The response must be one the document describes.
	vreq, _ := http.NewRequestWithContext(ctx, method, "http://localhost/"+strings.TrimPrefix(path, "/"), nil)
	vreq.URL.Path, _, _ = strings.Cut(path, "?")
	route, params, rerr := e.router.FindRoute(vreq)
	Expect(rerr).NotTo(HaveOccurred(), "the document describes %s %s", method, path)
	Expect(openapi3filter.ValidateResponse(ctx, &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request: vreq, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		},
		Status: resp.StatusCode, Header: resp.Header, Body: io.NopCloser(bytes.NewReader(raw)),
	})).To(Succeed(), "response %d %s", resp.StatusCode, raw)
	return resp.StatusCode, raw
}

func decode[T any](raw []byte) T {
	GinkgoHelper()
	var v T
	Expect(json.Unmarshal(raw, &v)).To(Succeed(), string(raw))
	return v
}

type providerJSON struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	HasKey          bool   `json:"hasKey"`
	Source          string `json:"source"`
	ManagedBySecret string `json:"managedBySecret"`
	DisabledReason  string `json:"disabledReason"`

	AnthropicWorkspaceID string   `json:"anthropicWorkspaceId"`
	HasCostKey           bool     `json:"hasCostKey"`
	CostScope            string   `json:"costScope"`
	Formats              []string `json:"formats"`
}

type modelJSON struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Guard            bool   `json:"guard"`
	Source           string `json:"source"`
	Format           string `json:"format"`
	FormatOverridden bool   `json:"formatOverridden"`
}

type tokenJSON struct {
	ID         string     `json:"id"`
	Prefix     string     `json:"prefix"`
	Role       string     `json:"role"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	CreatedAt  time.Time  `json:"createdAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
}

var _ = Describe("the admin API", func() {
	var e *env
	BeforeEach(func(ctx SpecContext) { e = newEnv(ctx) })

	It("serves its OpenAPI document without a token", func(ctx SpecContext) {
		resp, err := http.Get(e.srv.URL + "/admin/v1/openapi.yaml")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		Expect(resp.StatusCode).To(Equal(200))
		Expect(string(b)).To(HavePrefix("# SPDX"))
		Expect(string(b)).To(ContainSubstring("openapi: 3.1.0"))
	})

	Describe("authentication", func() {
		It("takes bearer tokens only", func(ctx SpecContext) {
			for _, tok := range []string{"", "nonsense", "koinon_abcdefgh_wrong", e.token + "x"} {
				code, body := e.call(ctx, tok, "GET", "/providers", nil)
				Expect(code).To(Equal(401), tok)
				Expect(string(body)).To(ContainSubstring("unauthenticated"))
			}
			req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/admin/v1/providers", nil)
			req.AddCookie(&http.Cookie{Name: "koinon_session", Value: "anything"})
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			_ = resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(401), "a session cookie is not accepted")
		})

		It("refuses expired and revoked tokens", func(ctx SpecContext) {
			code, _ := e.call(ctx, e.token, "GET", "/providers", nil)
			Expect(code).To(Equal(200))

			created := decode[struct {
				Token  tokenJSON `json:"token"`
				Secret string    `json:"secret"`
			}](mustBody(e.call(ctx, e.token, "POST", "/tokens", map[string]any{"name": "short", "role": "admin", "expiresIn": 3600})))
			code, _ = e.call(ctx, created.Secret, "GET", "/providers", nil)
			Expect(code).To(Equal(200))
			later := time.Now().Add(2 * time.Hour)
			e.now.Store(&later)
			code, _ = e.call(ctx, created.Secret, "GET", "/providers", nil)
			Expect(code).To(Equal(401), "expired")
			e.now.Store(nil)

			code, _ = e.call(ctx, e.token, "DELETE", "/tokens/"+created.Token.ID, nil)
			Expect(code).To(Equal(204))
			code, _ = e.call(ctx, e.token, "DELETE", "/tokens/"+created.Token.ID, nil)
			Expect(code).To(Equal(204), "revoking twice is fine")
			fresh := time.Now()
			e.now.Store(&fresh)
			code, _ = e.call(ctx, created.Secret, "GET", "/providers", nil)
			Expect(code).To(Equal(401), "revoked")
		})
	})

	Describe("tokens", func() {
		It("expire in 30 days by default, at most 90, and are shown once", func(ctx SpecContext) {
			type created struct {
				Token  tokenJSON `json:"token"`
				Secret string    `json:"secret"`
			}
			c := decode[created](mustBody(e.call(ctx, e.token, "POST", "/tokens", map[string]any{"name": "ci", "role": "admin"})))
			Expect(c.Secret).To(HavePrefix("koinon_" + c.Token.Prefix + "_"))
			Expect(c.Token.ExpiresAt.Sub(c.Token.CreatedAt)).To(BeNumerically("~", 30*24*time.Hour, time.Second))

			code, _ := e.call(ctx, e.token, "POST", "/tokens", map[string]any{"name": "long", "role": "admin", "expiresIn": 91 * 24 * 3600})
			Expect(code).To(Equal(400))
			code, _ = e.call(ctx, e.token, "POST", "/tokens", map[string]any{"name": "short", "role": "admin", "expiresIn": 60})
			Expect(code).To(Equal(400))
			code, _ = e.call(ctx, e.token, "POST", "/tokens", map[string]any{"name": "viewer", "role": "viewer"})
			Expect(code).To(Equal(400))

			code, body := e.call(ctx, e.token, "GET", "/tokens", nil)
			Expect(code).To(Equal(200))
			Expect(string(body)).NotTo(ContainSubstring(c.Secret))
			var hash []byte
			Expect(pool.QueryRow(ctx, `SELECT token_hash FROM api_tokens WHERE id = $1`, c.Token.ID).Scan(&hash)).To(Succeed())
			Expect(hash).To(HaveLen(32))
			Expect(string(hash)).NotTo(ContainSubstring(c.Secret))
		})

		It("never carry more than the creator's role", func(ctx SpecContext) {
			admin := decode[struct{ Secret string }](mustBody(e.call(ctx, e.token, "POST", "/tokens", map[string]any{"name": "admin", "role": "admin"})))
			code, body := e.call(ctx, admin.Secret, "POST", "/tokens", map[string]any{"name": "escalate", "role": "owner"})
			Expect(code).To(Equal(400))
			Expect(string(body)).To(ContainSubstring("higher role"))
			code, _ = e.call(ctx, admin.Secret, "POST", "/tokens", map[string]any{"name": "same", "role": "admin"})
			Expect(code).To(Equal(201))
		})

		It("record when they were last used", func(ctx SpecContext) {
			c := decode[struct {
				Token  tokenJSON
				Secret string
			}](mustBody(e.call(ctx, e.token, "POST", "/tokens", map[string]any{"name": "ci", "role": "admin"})))
			Expect(c.Token.LastUsedAt).To(BeNil())
			e.call(ctx, c.Secret, "GET", "/models", nil)
			list := decode[struct{ Tokens []tokenJSON }](mustBody(e.call(ctx, e.token, "GET", "/tokens", nil)))
			for _, t := range list.Tokens {
				if t.ID == c.Token.ID {
					Expect(t.LastUsedAt).NotTo(BeNil())
				}
			}
		})

		type made struct {
			Token  tokenJSON `json:"token"`
			Secret string    `json:"secret"`
		}
		mint := func(ctx SpecContext, e *env, by, role string, body map[string]any) (int, made) {
			GinkgoHelper()
			body["name"], body["role"] = "t", role
			code, raw := e.call(ctx, by, "POST", "/tokens", body)
			var m made
			if code == 201 {
				m = decode[made](raw)
			}
			return code, m
		}

		It("cannot extend themselves: a child expires no later than its parent", func(ctx SpecContext) {
			_, root := mint(ctx, e, e.token, "admin", map[string]any{"expiresIn": 7 * 24 * 3600})
			code, _ := mint(ctx, e, root.Secret, "admin", map[string]any{"expiresIn": 30 * 24 * 3600})
			Expect(code).To(Equal(400), "an explicit expiry past the parent's")
			code, child := mint(ctx, e, root.Secret, "admin", map[string]any{})
			Expect(code).To(Equal(201))
			Expect(child.Token.ExpiresAt).To(BeTemporally("<=", root.Token.ExpiresAt))
			code, grand := mint(ctx, e, child.Secret, "admin", map[string]any{})
			Expect(code).To(Equal(201))
			Expect(grand.Token.ExpiresAt).To(BeTemporally("<=", root.Token.ExpiresAt))

			By("a token made from a session starts a fresh chain with the full default")
			_, fresh := mint(ctx, e, e.token, "admin", map[string]any{})
			Expect(fresh.Token.ExpiresAt.Sub(fresh.Token.CreatedAt)).To(BeNumerically("~", 30*24*time.Hour, time.Second))
		})

		It("carry the root user down the chain, and parent links", func(ctx SpecContext) {
			_, a := mint(ctx, e, e.token, "admin", map[string]any{})
			_, b := mint(ctx, e, a.Secret, "admin", map[string]any{})
			var by pgtype.UUID
			var parent pgtype.UUID
			Expect(pool.QueryRow(ctx, `SELECT created_by, parent_token_id FROM api_tokens WHERE id = $1`, b.Token.ID).Scan(&by, &parent)).To(Succeed())
			Expect(by).To(Equal(e.user))
			Expect(parent.String()).To(Equal(a.Token.ID))
		})

		It("are rejected once their creator no longer holds the role, and the failure is audited", func(ctx SpecContext) {
			_, a := mint(ctx, e, e.token, "admin", map[string]any{})
			_, child := mint(ctx, e, a.Secret, "admin", map[string]any{})
			code, _ := e.call(ctx, child.Secret, "GET", "/providers", nil)
			Expect(code).To(Equal(200))

			// An org keeps one manual owner: make someone else the owner, then demote the creator.
			_, other, _ := pgtest.NewOrg(ctx, pool)
			_, err := pool.Exec(ctx, `INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'manual', 'owner')`, e.org, other)
			Expect(err).NotTo(HaveOccurred())
			_, err = pool.Exec(ctx, `UPDATE org_memberships SET role = 'member' WHERE org_id = $1 AND user_id = $2`, e.org, e.user)
			Expect(err).NotTo(HaveOccurred())
			for _, tok := range []string{child.Secret, a.Secret, e.token} {
				code, _ = e.call(ctx, tok, "GET", "/providers", nil)
				Expect(code).To(Equal(401), "demoted")
			}
			var n int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'token.use_failed' AND target_id = $2 AND summary::text LIKE '%no longer holds%'`,
				e.org, child.Token.ID).Scan(&n)).To(Succeed())
			Expect(n).To(Equal(1))

			By("an admin keeps an admin token, and restoring the role restores the chain")
			_, err = pool.Exec(ctx, `UPDATE org_memberships SET role = 'owner' WHERE org_id = $1 AND user_id = $2`, e.org, e.user)
			Expect(err).NotTo(HaveOccurred())
			code, _ = e.call(ctx, child.Secret, "GET", "/providers", nil)
			Expect(code).To(Equal(200))
		})

		It("revoke their descendants too", func(ctx SpecContext) {
			_, a := mint(ctx, e, e.token, "admin", map[string]any{})
			_, b := mint(ctx, e, a.Secret, "admin", map[string]any{})
			_, c := mint(ctx, e, b.Secret, "admin", map[string]any{})
			_, sibling := mint(ctx, e, e.token, "admin", map[string]any{})
			code, _ := e.call(ctx, e.token, "DELETE", "/tokens/"+a.Token.ID, nil)
			Expect(code).To(Equal(204))
			for _, tok := range []string{a.Secret, b.Secret, c.Secret} {
				code, _ = e.call(ctx, tok, "GET", "/providers", nil)
				Expect(code).To(Equal(401))
			}
			code, _ = e.call(ctx, sibling.Secret, "GET", "/providers", nil)
			Expect(code).To(Equal(200), "tokens outside the subtree are untouched")
			code, _ = e.call(ctx, e.token, "GET", "/providers", nil)
			Expect(code).To(Equal(200))
		})

		It("cannot revoke a token with a higher role", func(ctx SpecContext) {
			_, admin := mint(ctx, e, e.token, "admin", map[string]any{})
			code, body := e.call(ctx, admin.Secret, "DELETE", "/tokens/"+decode[[]struct{ ID string }](mustList(ctx, e))[0].ID, nil)
			Expect(code).To(Equal(400), string(body))
		})

		It("slows down a client that keeps sending bad tokens", func(ctx SpecContext) {
			var last int
			for range 8 {
				last, _ = e.call(ctx, "koinon_abcdefgh_wrong", "GET", "/providers", nil)
			}
			Expect(last).To(Equal(429))
			code, _ := e.call(ctx, e.token, "GET", "/providers", nil)
			Expect(code).To(Equal(200), "a valid token is never locked out by bad ones")
			_, other := mint(ctx, e, e.token, "admin", map[string]any{})
			code, _ = e.call(ctx, other.Secret, "GET", "/providers", nil)
			Expect(code).To(Equal(200))
		})

		It("slows down guesses at a real token's secret, but not that token's valid use", func(ctx SpecContext) {
			_, victim := mint(ctx, e, e.token, "admin", map[string]any{})
			guess := "koinon_" + victim.Token.Prefix + "_wrong-secret"
			var last int
			for range 8 {
				last, _ = e.call(ctx, guess, "GET", "/providers", nil)
			}
			Expect(last).To(Equal(429))
			code, _ := e.call(ctx, victim.Secret, "GET", "/providers", nil)
			Expect(code).To(Equal(200), "the real holder still gets in")
			var n int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'token.use_failed' AND target_id = $2`, e.org, victim.Token.ID).Scan(&n)).To(Succeed())
			Expect(n).To(BeNumerically(">=", 6))
		})

		It("cannot create a child while the parent is being revoked", func(ctx SpecContext) {
			_, parent := mint(ctx, e, e.token, "admin", map[string]any{})
			done := make(chan []made, 1)
			go func() {
				defer GinkgoRecover()
				var kids []made
				for range 6 {
					code, k := mint(ctx, e, parent.Secret, "admin", map[string]any{})
					if code == 201 {
						kids = append(kids, k)
					}
				}
				done <- kids
			}()
			time.Sleep(15 * time.Millisecond)
			code, _ := e.call(ctx, e.token, "DELETE", "/tokens/"+parent.Token.ID, nil)
			Expect(code).To(Equal(204))
			kids := <-done
			for _, k := range kids {
				code, _ := e.call(ctx, k.Secret, "GET", "/providers", nil)
				Expect(code).To(Equal(401), "every child that got created is revoked with its parent")
			}
		})

		It("are scoped to their org", func(ctx SpecContext) {
			other := newEnv(ctx)
			c := decode[struct{ Token tokenJSON }](mustBody(e.call(ctx, e.token, "POST", "/tokens", map[string]any{"name": "mine", "role": "admin"})))
			code, _ := other.call(ctx, other.token, "DELETE", "/tokens/"+c.Token.ID, nil)
			Expect(code).To(Equal(404), "another org's token is not found")
			list := decode[struct{ Tokens []tokenJSON }](mustBody(other.call(ctx, other.token, "GET", "/tokens", nil)))
			Expect(list.Tokens).To(HaveLen(1))
		})
	})

	Describe("providers and models", func() {
		It("sets and validates a model's format and an Anthropic provider's workspace ID (ADR 0069)", func(ctx SpecContext) {
			code, body := e.call(ctx, e.token, "POST", "/providers", map[string]any{
				"name": "anthropic", "type": "anthropic", "url": "https://api.anthropic.com/v1", "anthropicWorkspaceId": "wrkspc_01Ab",
			})
			Expect(code).To(Equal(201), string(body))
			p := decode[providerJSON](body)
			Expect(p.AnthropicWorkspaceID).To(Equal("wrkspc_01Ab"))
			Expect(p.Formats).To(Equal([]string{"anthropic"}))

			code, body = e.call(ctx, e.token, "POST", "/models", map[string]any{"providerId": p.ID, "name": "claude-haiku-4-5"})
			Expect(code).To(Equal(201), string(body))
			m := decode[modelJSON](body)
			Expect(m.Format).To(Equal("anthropic"))
			Expect(m.FormatOverridden).To(BeFalse())

			code, body = e.call(ctx, e.token, "POST", "/models", map[string]any{"providerId": p.ID, "name": "other", "format": "openai"})
			Expect(code).To(Equal(400), string(body))
			Expect(string(body)).To(ContainSubstring("does not offer"))
			code, body = e.call(ctx, e.token, "PATCH", "/models/"+m.ID, map[string]any{"format": "anthropic"})
			Expect(code).To(Equal(200), string(body))
			Expect(decode[modelJSON](body).FormatOverridden).To(BeTrue())
			code, body = e.call(ctx, e.token, "PATCH", "/models/"+m.ID, map[string]any{"format": ""})
			Expect(code).To(Equal(200), string(body))
			Expect(decode[modelJSON](body).FormatOverridden).To(BeFalse())
			code, _ = e.call(ctx, e.token, "POST", "/providers", map[string]any{
				"name": "oa", "type": "openai", "url": "https://api.openai.com/v1", "anthropicWorkspaceId": "wrkspc_01Ab",
			})
			Expect(code).To(Equal(400))
		})

		It("sets, replaces and removes a provider's cost API key on create and update, never returning it", func(ctx SpecContext) {
			const admin, rotated = "koinon_test_not-a-real-admin-key", "koinon_test_not-a-real-admin-key-2"
			code, body := e.call(ctx, e.token, "POST", "/providers", map[string]any{
				"name": "claude", "type": "anthropic", "url": "https://api.anthropic.com", "apiKey": "koinon_test_not-a-real-token",
				"costApiKey": "koinon_test_not-a-real-admin-key", "costScope": "wrkspc_01Ab",
			})
			Expect(code).To(Equal(201), string(body))
			Expect(string(body)).NotTo(ContainSubstring(admin))
			p := decode[providerJSON](body)
			Expect([]any{p.HasCostKey, p.CostScope}).To(Equal([]any{true, "wrkspc_01Ab"}))

			code, body = e.call(ctx, e.token, "PATCH", "/providers/"+p.ID, map[string]any{"costApiKey": rotated, "costScope": "default"})
			Expect(code).To(Equal(200), string(body))
			Expect(string(body)).NotTo(ContainSubstring(rotated))
			p = decode[providerJSON](body)
			Expect([]any{p.HasCostKey, p.CostScope}).To(Equal([]any{true, "default"}))
			p = decode[providerJSON](mustBody(e.call(ctx, e.token, "GET", "/providers/"+p.ID, nil)))
			Expect(p.HasCostKey).To(BeTrue(), "a read shows the flag, not the key")

			code, body = e.call(ctx, e.token, "PATCH", "/providers/"+p.ID, map[string]any{"costApiKey": ""})
			Expect(code).To(Equal(200), string(body))
			p = decode[providerJSON](body)
			Expect([]any{p.HasCostKey, p.CostScope}).To(Equal([]any{false, ""}))

			code, body = e.call(ctx, e.token, "POST", "/providers", map[string]any{
				"name": "or", "type": "openrouter", "url": "https://openrouter.ai/api/v1", "costApiKey": admin, "costScope": "proj_abc",
			})
			Expect(code).To(Equal(400), string(body))
			code, body = e.call(ctx, e.token, "POST", "/providers", map[string]any{
				"name": "noscope", "type": "anthropic", "url": "https://api.anthropic.com", "costApiKey": admin,
			})
			Expect(code).To(Equal(400), string(body))
		})

		It("create, read, change and delete, never returning the key", func(ctx SpecContext) {
			code, body := e.call(ctx, e.token, "POST", "/providers", map[string]any{
				"name": "openrouter", "type": "openrouter", "url": "https://openrouter.ai/api/v1", "apiKey": "sk-or-secret",
			})
			Expect(code).To(Equal(201), string(body))
			Expect(string(body)).NotTo(ContainSubstring("sk-or-secret"))
			p := decode[providerJSON](body)
			Expect(p.HasKey).To(BeTrue())
			Expect(p.Source).To(Equal("ui"))

			code, body = e.call(ctx, e.token, "POST", "/models", map[string]any{
				"providerId": p.ID, "name": "deepseek/flash", "guard": true,
			})
			Expect(code).To(Equal(201), string(body))
			m := decode[modelJSON](body)
			Expect(m.Guard).To(BeTrue())

			code, body = e.call(ctx, e.token, "POST", "/models", map[string]any{
				"providerId": p.ID, "name": "with-rates", "rates": map[string]any{"input": "0.30", "output": "1.20"},
			})
			Expect(code).To(Equal(400), "rates on a model were replaced by prices")
			Expect(string(body)).To(ContainSubstring("prices"))
			code, _ = e.call(ctx, e.token, "PATCH", "/models/"+m.ID, map[string]any{"clearRates": true})
			Expect(code).To(Equal(400))

			code, body = e.call(ctx, e.token, "PATCH", "/models/"+m.ID, map[string]any{"guard": false})
			Expect(code).To(Equal(200), string(body))
			Expect(decode[modelJSON](body).Guard).To(BeFalse())

			code, body = e.call(ctx, e.token, "PATCH", "/providers/"+p.ID, map[string]any{"apiKey": ""})
			Expect(code).To(Equal(200), string(body))
			Expect(decode[providerJSON](body).HasKey).To(BeFalse())

			code, _ = e.call(ctx, e.token, "GET", "/providers/"+p.ID, nil)
			Expect(code).To(Equal(200))
			code, _ = e.call(ctx, e.token, "DELETE", "/models/"+m.ID, nil)
			Expect(code).To(Equal(204))
			code, _ = e.call(ctx, e.token, "DELETE", "/providers/"+p.ID, nil)
			Expect(code).To(Equal(204))
			code, _ = e.call(ctx, e.token, "GET", "/providers/"+p.ID, nil)
			Expect(code).To(Equal(404))
		})

		It("answer 400 for invalid input and 409 for a taken name", func(ctx SpecContext) {
			code, _ := e.call(ctx, e.token, "POST", "/providers", map[string]any{"name": "Bad Name", "type": "openai", "url": "https://x.example.com/v1"})
			Expect(code).To(Equal(400))
			code, _ = e.call(ctx, e.token, "POST", "/providers", map[string]any{"name": "a", "type": "openai", "url": "ftp://x"})
			Expect(code).To(Equal(400))
			req, _ := http.NewRequestWithContext(ctx, "POST", e.srv.URL+"/admin/v1/providers", strings.NewReader(`{"name":`))
			req.Header.Set("Authorization", "Bearer "+e.token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			_ = resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(400))

			ok := map[string]any{"name": "dup", "type": "openai", "url": "https://x.example.com/v1"}
			code, _ = e.call(ctx, e.token, "POST", "/providers", ok)
			Expect(code).To(Equal(201))
			code, _ = e.call(ctx, e.token, "POST", "/providers", ok)
			Expect(code).To(Equal(409))
		})

		It("show declared entries as read-only, answering 409 with the Secret's name", func(ctx SpecContext) {
			_, err := e.catalog.ApplyDeclaredProvider(ctx, e.org, catalog.DeclaredProvider{
				SecretName: "declared-1", Version: "v1", Name: "declared", Type: "ollama", URL: "http://ollama.example.com:11434/v1",
				Models: []catalog.DeclaredModel{{Name: "qwen"}},
			})
			Expect(err).NotTo(HaveOccurred())
			list := decode[struct{ Providers []providerJSON }](mustBody(e.call(ctx, e.token, "GET", "/providers", nil)))
			Expect(list.Providers).To(HaveLen(1))
			Expect(list.Providers[0].Source).To(Equal("secret"))
			Expect(list.Providers[0].ManagedBySecret).To(Equal("declared-1"))
			models := decode[struct{ Models []modelJSON }](mustBody(e.call(ctx, e.token, "GET", "/models", nil)))

			code, body := e.call(ctx, e.token, "PATCH", "/providers/"+list.Providers[0].ID, map[string]any{"url": "http://x.example.com/v1"})
			Expect(code).To(Equal(409))
			Expect(string(body)).To(ContainSubstring("declared-1"))
			code, _ = e.call(ctx, e.token, "DELETE", "/providers/"+list.Providers[0].ID, nil)
			Expect(code).To(Equal(409))
			code, _ = e.call(ctx, e.token, "PATCH", "/models/"+models.Models[0].ID, map[string]any{"guard": false})
			Expect(code).To(Equal(409))
			code, _ = e.call(ctx, e.token, "DELETE", "/models/"+models.Models[0].ID, nil)
			Expect(code).To(Equal(409))
		})

		It("never show or change another org's entries", func(ctx SpecContext) {
			other := newEnv(ctx)
			code, body := other.call(ctx, other.token, "POST", "/providers", map[string]any{"name": "theirs", "type": "openai", "url": "https://x.example.com/v1"})
			Expect(code).To(Equal(201))
			theirs := decode[providerJSON](body)

			list := decode[struct{ Providers []providerJSON }](mustBody(e.call(ctx, e.token, "GET", "/providers", nil)))
			Expect(list.Providers).To(BeEmpty())
			code, _ = e.call(ctx, e.token, "GET", "/providers/"+theirs.ID, nil)
			Expect(code).To(Equal(404))
			code, _ = e.call(ctx, e.token, "PATCH", "/providers/"+theirs.ID, map[string]any{"url": "http://evil.example.com/v1"})
			Expect(code).To(Equal(404))
			code, _ = e.call(ctx, e.token, "DELETE", "/providers/"+theirs.ID, nil)
			Expect(code).To(Equal(404))
			code, _ = e.call(ctx, e.token, "POST", "/models", map[string]any{"providerId": theirs.ID, "name": "m"})
			Expect(code).To(Equal(404))
		})
	})

	It("audits every call with the token's ID, and changes with their details", func(ctx SpecContext) {
		c := decode[struct{ Token tokenJSON }](mustBody(e.call(ctx, e.token, "POST", "/tokens", map[string]any{"name": "ci", "role": "admin"})))
		var rootID string
		Expect(pool.QueryRow(ctx, `SELECT id::text FROM api_tokens WHERE org_id = $1 AND name = 'bootstrap'`, e.org).Scan(&rootID)).To(Succeed())

		e.call(ctx, e.token, "GET", "/providers", nil)
		e.call(ctx, e.token, "POST", "/providers", map[string]any{"name": "Bad", "type": "x", "url": "y"})
		code, body := e.call(ctx, e.token, "POST", "/providers", map[string]any{"name": "ok", "type": "openai", "url": "https://x.example.com/v1", "apiKey": "sk-secret-1"})
		Expect(code).To(Equal(201), string(body))

		rows, err := store.New(pool).ListAuditEvents(ctx, store.ListAuditEventsParams{OrgID: e.org, PageSize: 100})
		Expect(err).NotTo(HaveOccurred())
		var calls []string
		var created bool
		for _, r := range rows {
			Expect(string(r.Summary)).NotTo(ContainSubstring("sk-secret-1"))
			if r.Action == "admin_api.call" {
				Expect(r.ActorKind).To(Equal("token"))
				Expect(r.ActorID).To(Equal(rootID))
				Expect(r.RequestID).To(HavePrefix("req-"))
				calls = append(calls, string(r.Summary))
			}
			if r.Action == "provider.create" {
				created = true
				Expect(r.ActorKind).To(Equal("token"))
				Expect(r.RequestID).To(HavePrefix("req-"))
			}
		}
		Expect(created).To(BeTrue())
		Expect(len(calls)).To(BeNumerically(">=", 4))
		Expect(strings.Join(calls, " ")).To(And(ContainSubstring(`"status": 400`), ContainSubstring(`"status": 201`)))
		_ = c
	})
})

func mustBody(_ int, body []byte) []byte { return body }

// mustList returns the root (first created) token of the env's org, as JSON {ID}.
func mustList(ctx SpecContext, e *env) []byte {
	GinkgoHelper()
	var id string
	Expect(pool.QueryRow(ctx, `SELECT id::text FROM api_tokens WHERE org_id = $1 AND name = 'bootstrap'`, e.org).Scan(&id)).To(Succeed())
	b, _ := json.Marshal([]map[string]string{{"ID": id}})
	return b
}
