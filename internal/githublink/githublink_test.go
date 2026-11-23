// SPDX-License-Identifier: Apache-2.0

package githublink_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/githublink"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
)

var seq atomic.Int64

func uniq(p string) string { return fmt.Sprintf("%s-%d-%d", p, time.Now().UnixNano(), seq.Add(1)) }

// sessions stands in for the browser session: the cookie value names the signed-in user.
type sessions struct{}

func (sessions) AuthenticateRequest(r *http.Request) (*auth.Principal, error) {
	c, err := r.Cookie("session")
	if err != nil {
		return nil, auth.ErrUnauthenticated
	}
	var id pgtype.UUID
	if err := id.Scan(c.Value); err != nil {
		return nil, auth.ErrUnauthenticated
	}
	// The memberships as the real session loads them, so a user who left the org is not a member.
	p := &auth.Principal{UserID: id}
	rows, err := pool.Query(r.Context(), `SELECT org_id, role FROM org_memberships WHERE user_id = $1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m auth.Membership
		if err := rows.Scan(&m.OrgID, &m.Role); err != nil {
			return nil, err
		}
		p.Memberships = append(p.Memberships, m)
	}
	return p, rows.Err()
}

const publicURL = "https://koinon.example.test"

type fixture struct {
	org          pgtype.UUID
	olive, mia   pgtype.UUID
	conn, noAuth pgtype.UUID
	gh           *githubtest.Fake
	svc          *githublink.Service
	ring         *secretstore.Keyring
	now          time.Time
}

func newFixture(ctx context.Context) *fixture {
	GinkgoHelper()
	f := &fixture{gh: githubtest.New(), now: time.Now()}
	DeferCleanup(f.gh.Server.Close)
	user := func(tx pgx.Tx, name string, role store.OrgRole) pgtype.UUID {
		var personal, id pgtype.UUID
		Expect(tx.QueryRow(ctx, `INSERT INTO orgs (slug, display_name, kind) VALUES ($1, 'P', 'personal') RETURNING id`, uniq("p")).Scan(&personal)).To(Succeed())
		Expect(tx.QueryRow(ctx,
			`INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id, display_name, email)
			 VALUES ('https://idp.test', $1, $2, $3, lower($3) || '@example.test') RETURNING id`,
			uniq("sub"), personal, name).Scan(&id)).To(Succeed())
		_, err := tx.Exec(ctx, `INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'manual', $3), ($4, $2, 'manual', 'owner')`, f.org, id, role, personal)
		Expect(err).NotTo(HaveOccurred())
		return id
	}
	Expect(pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO orgs (slug, display_name, kind) VALUES ($1, 'Team', 'team') RETURNING id`, uniq("org")).Scan(&f.org); err != nil {
			return err
		}
		f.olive = user(tx, "Olive", store.OrgRoleOwner)
		f.mia = user(tx, "Mia", store.OrgRoleMember)
		return nil
	})).To(Succeed())

	ring := secretstoretest.Keyring()
	f.ring = ring
	q := store.New(pool)
	add := func(clientID string, fields map[string][]byte) pgtype.UUID {
		id := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		env, err := connections.SealFields(ring, f.org, id, fields)
		Expect(err).NotTo(HaveOccurred())
		cfg, _ := json.Marshal(github.Config{AppID: f.gh.AppID, InstallationID: f.gh.InstallationID, ClientID: clientID})
		_, err = q.CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
			OrgID: f.org, ID: id, Connector: "github", DisplayName: uniq("acme"),
			Config: cfg, SecretEnvelope: env, Source: store.ConfigSourceUi, CreatedBy: f.olive,
		})
		Expect(err).NotTo(HaveOccurred())
		return id
	}
	f.conn = add(githubtest.FakeClientID, map[string][]byte{
		connections.GitHubPrivateKeyField: f.gh.PEM, github.ClientSecretField: []byte(githubtest.FakeClientSecret),
	})
	f.noAuth = add("", map[string][]byte{connections.GitHubPrivateKeyField: f.gh.PEM})

	f.svc = githublink.New(pool, githublink.Options{
		Keys: ring, Hosts: providers.New(github.NewClient(github.Options{BaseURL: f.gh.URL()})),
		PublicURL: publicURL + "/", Auth: sessions{}, Now: func() time.Time { return f.now },
	})
	return f
}

// callback sends the browser's request to the callback as user (no user: no session) and
// returns the result the redirect names.
func (f *fixture) callback(ctx context.Context, back string, user pgtype.UUID) string {
	GinkgoHelper()
	u, err := url.Parse(back)
	Expect(err).NotTo(HaveOccurred())
	Expect(u.Scheme + "://" + u.Host + u.Path).To(Equal(publicURL + githublink.CallbackPath))
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, u.RequestURI(), nil)
	if user.Valid {
		r.AddCookie(&http.Cookie{Name: "session", Value: user.String()})
	}
	w := httptest.NewRecorder()
	mux := http.NewServeMux()
	f.svc.Mount(mux)
	mux.ServeHTTP(w, r)
	Expect(w.Code).To(Equal(http.StatusSeeOther))
	Expect(w.Header().Get("Cache-Control")).To(Equal("no-store"))
	loc, err := url.Parse(w.Header().Get("Location"))
	Expect(err).NotTo(HaveOccurred())
	Expect(loc.Path).To(Equal("/connections"))
	return loc.Query().Get("github_link")
}

// start begins a link for user and plays GitHub's consent page for the GitHub user.
func (f *fixture) start(ctx context.Context, user pgtype.UUID, code string, gu githubtest.GitHubUser) string {
	GinkgoHelper()
	authURL, err := f.svc.Start(ctx, f.org, user, f.conn)
	Expect(err).NotTo(HaveOccurred())
	back, err := f.gh.Consent(authURL, code, gu)
	Expect(err).NotTo(HaveOccurred())
	return back
}

func (f *fixture) auditActions(ctx context.Context) []string {
	GinkgoHelper()
	rows, err := pool.Query(ctx, `SELECT action || ' ' || actor_id || ' ' || target_id || ' ' || summary::text FROM audit_events
		WHERE org_id = $1 AND action LIKE 'github_link.%' ORDER BY created_at, id`, f.org)
	Expect(err).NotTo(HaveOccurred())
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	Expect(err).NotTo(HaveOccurred())
	return out
}

var oliveGH = githubtest.GitHubUser{ID: 4101, Login: "olive-gh"}

var _ = Describe("GitHub link", func() {
	It("links the user's login, revokes the user token and audits the link", func(ctx SpecContext) {
		f := newFixture(ctx)
		authURL, err := f.svc.Start(ctx, f.org, f.olive, f.conn)
		Expect(err).NotTo(HaveOccurred())

		By("the authorize page is the App's, with state, PKCE S256 and Koinon's callback")
		u, err := url.Parse(authURL)
		Expect(err).NotTo(HaveOccurred())
		Expect(u.Scheme + "://" + u.Host + u.Path).To(Equal(f.gh.WebURL() + "/login/oauth/authorize"))
		Expect(u.Query().Get("client_id")).To(Equal(githubtest.FakeClientID))
		Expect(u.Query().Get("redirect_uri")).To(Equal(publicURL + "/auth/github/link/callback"))
		Expect(u.Query().Get("code_challenge_method")).To(Equal("S256"))
		Expect(u.Query().Get("state")).To(HavePrefix(f.org.String() + "."))
		Expect(u.Query().Get("allow_signup")).To(Equal("false"))

		By("only the hash of the state is stored")
		var stored int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM github_link_flows WHERE org_id = $1 AND user_id = $2 AND state_hash::text LIKE '%' || $3 || '%'`,
			f.org, f.olive, u.Query().Get("state")).Scan(&stored)).To(Succeed())
		Expect(stored).To(BeZero())

		back, err := f.gh.Consent(authURL, "code-1", oliveGH)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.callback(ctx, back, f.olive)).To(Equal("linked"))

		links, err := f.svc.Mine(ctx, f.org, f.olive)
		Expect(err).NotTo(HaveOccurred())
		Expect(links).To(HaveLen(1))
		Expect(links[0].Host).To(Equal(f.gh.WebURL()))
		Expect(links[0].GitHubLogin).To(Equal("olive-gh"))
		Expect(links[0].GitHubUserID).To(BeEquivalentTo(4101))

		By("the user token was revoked and is stored nowhere")
		Expect(f.gh.UserTokens()).To(HaveLen(1))
		for tok, revoked := range f.gh.UserTokens() {
			Expect(revoked).To(BeTrue(), "token %s", tok)
		}
		var dump string
		Expect(pool.QueryRow(ctx, `SELECT coalesce(string_agg(t::text, ''), '') FROM (
			SELECT row_to_json(l)::text AS t FROM github_user_links l WHERE org_id = $1
			UNION ALL SELECT row_to_json(a)::text FROM audit_events a WHERE org_id = $1) x`, f.org).Scan(&dump)).To(Succeed())
		Expect(dump).NotTo(ContainSubstring("ghu_"))

		By("the link is audited")
		Expect(f.auditActions(ctx)).To(ConsistOf(
			ContainSubstring(`github_link.create ` + f.olive.String() + ` ` + f.olive.String() + ` {"after": {"host": "` + f.gh.WebURL() + `", "githubLogin": "olive-gh", "githubUserId": 4101}`),
		))

		By("the state works once")
		Expect(f.callback(ctx, back, f.olive)).To(Equal("expired"))
	})

	It("refuses a state that another user sends, and keeps it for the user who started it", func(ctx SpecContext) {
		f := newFixture(ctx)
		back := f.start(ctx, f.olive, "code-2", oliveGH)
		Expect(f.callback(ctx, back, f.mia)).To(Equal("expired"))
		mine, err := f.svc.Mine(ctx, f.org, f.mia)
		Expect(err).NotTo(HaveOccurred())
		Expect(mine).To(BeEmpty())
		Expect(f.callback(ctx, back, f.olive)).To(Equal("linked"))
	})

	It("asks to sign in when the browser has no session", func(ctx SpecContext) {
		f := newFixture(ctx)
		back := f.start(ctx, f.olive, "code-3", oliveGH)
		Expect(f.callback(ctx, back, pgtype.UUID{})).To(Equal("signin"))
	})

	It("refuses a user who left the org during the sign-in", func(ctx SpecContext) {
		f := newFixture(ctx)
		back := f.start(ctx, f.mia, "code-left", oliveGH)
		_, err := pool.Exec(ctx, `DELETE FROM org_memberships WHERE org_id = $1 AND user_id = $2`, f.org, f.mia)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.callback(ctx, back, f.mia)).To(Equal("expired"))
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM github_user_links WHERE org_id = $1`, f.org).Scan(&n)).To(Succeed())
		Expect(n).To(BeZero())
	})

	It("refuses an expired state", func(ctx SpecContext) {
		f := newFixture(ctx)
		back := f.start(ctx, f.olive, "code-4", oliveGH)
		f.now = f.now.Add(githublink.FlowTTL + time.Second)
		Expect(f.callback(ctx, back, f.olive)).To(Equal("expired"))
	})

	It("refuses a state of another org or a forged state", func(ctx SpecContext) {
		f := newFixture(ctx)
		other := newFixture(ctx)
		back := f.start(ctx, f.olive, "code-5", oliveGH)
		u, _ := url.Parse(back)
		q := u.Query()
		_, rest, _ := strings.Cut(q.Get("state"), ".")
		for _, state := range []string{other.org.String() + "." + rest, "not-a-state", ""} {
			q.Set("state", state)
			u.RawQuery = q.Encode()
			Expect(f.callback(ctx, u.String(), f.olive)).To(Equal("expired"), state)
		}
	})

	It("records nothing when the user does not authorize the App", func(ctx SpecContext) {
		f := newFixture(ctx)
		authURL, err := f.svc.Start(ctx, f.org, f.olive, f.conn)
		Expect(err).NotTo(HaveOccurred())
		u, _ := url.Parse(authURL)
		back := publicURL + githublink.CallbackPath + "?" + url.Values{"error": {"access_denied"}, "state": {u.Query().Get("state")}}.Encode()
		Expect(f.callback(ctx, back, f.olive)).To(Equal("denied"))
		Expect(f.svc.Mine(ctx, f.org, f.olive)).To(BeEmpty())
		Expect(f.auditActions(ctx)).To(BeEmpty())
	})

	It("fails when GitHub refuses the code", func(ctx SpecContext) {
		f := newFixture(ctx)
		authURL, err := f.svc.Start(ctx, f.org, f.olive, f.conn)
		Expect(err).NotTo(HaveOccurred())
		u, _ := url.Parse(authURL)
		back := publicURL + githublink.CallbackPath + "?" + url.Values{"code": {"never-issued"}, "state": {u.Query().Get("state")}}.Encode()
		Expect(f.callback(ctx, back, f.olive)).To(Equal("failed"))
		Expect(f.svc.Mine(ctx, f.org, f.olive)).To(BeEmpty())
	})

	It("refuses a GitHub account that another user of the org linked", func(ctx SpecContext) {
		f := newFixture(ctx)
		Expect(f.callback(ctx, f.start(ctx, f.olive, "code-6", oliveGH), f.olive)).To(Equal("linked"))
		Expect(f.callback(ctx, f.start(ctx, f.mia, "code-7", oliveGH), f.mia)).To(Equal("taken"))
		Expect(f.svc.Mine(ctx, f.org, f.mia)).To(BeEmpty())
	})

	It("replaces the user's link on the same host and audits both values", func(ctx SpecContext) {
		f := newFixture(ctx)
		Expect(f.callback(ctx, f.start(ctx, f.olive, "code-8", oliveGH), f.olive)).To(Equal("linked"))
		Expect(f.callback(ctx, f.start(ctx, f.olive, "code-9", githubtest.GitHubUser{ID: 4102, Login: "olive-new"}), f.olive)).To(Equal("linked"))
		links, err := f.svc.Mine(ctx, f.org, f.olive)
		Expect(err).NotTo(HaveOccurred())
		Expect(links).To(HaveLen(1))
		Expect(links[0].GitHubLogin).To(Equal("olive-new"))
		Expect(f.auditActions(ctx)).To(HaveLen(2))
		Expect(f.auditActions(ctx)[1]).To(ContainSubstring(`"before": {"host": "` + f.gh.WebURL() + `", "githubLogin": "olive-gh"`))
	})

	It("removes a link, audited, and lists the org's links with names", func(ctx SpecContext) {
		f := newFixture(ctx)
		Expect(f.callback(ctx, f.start(ctx, f.olive, "code-10", oliveGH), f.olive)).To(Equal("linked"))
		all, err := f.svc.List(ctx, f.org)
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(HaveLen(1))
		Expect(all[0].DisplayName).To(Equal("Olive"))
		Expect(all[0].Email).To(Equal("olive@example.test"))

		admin := audit.User(f.mia) // the caller checks the role; the actor is recorded
		Expect(f.svc.Delete(ctx, f.org, f.olive, f.gh.WebURL(), admin)).To(Succeed())
		Expect(f.svc.Delete(ctx, f.org, f.olive, f.gh.WebURL(), admin)).To(MatchError(githublink.ErrNotFound))
		Expect(f.svc.List(ctx, f.org)).To(BeEmpty())
		actions := f.auditActions(ctx)
		Expect(actions).To(HaveLen(2))
		Expect(actions[1]).To(HavePrefix("github_link.delete " + f.mia.String() + " " + f.olive.String()))
		Expect(actions[1]).To(ContainSubstring(`"githubLogin": "olive-gh"`))
	})

	It("refuses to start without the App's client ID and secret, a public URL or a known connection", func(ctx SpecContext) {
		f := newFixture(ctx)
		_, err := f.svc.Start(ctx, f.org, f.olive, f.noAuth)
		Expect(err).To(MatchError(githublink.ErrNoUserAuth))

		_, err = f.svc.Start(ctx, f.org, f.olive, pgtype.UUID{Bytes: uuid.New(), Valid: true})
		Expect(err).To(MatchError(githublink.ErrNotFound))

		other := newFixture(ctx)
		_, err = f.svc.Start(ctx, f.org, f.olive, other.conn)
		Expect(err).To(MatchError(githublink.ErrNotFound), "another org's connection")

		noURL := githublink.New(pool, githublink.Options{Keys: f.ring, Hosts: providers.New(github.NewClient(github.Options{BaseURL: f.gh.URL()}))})
		_, err = noURL.Start(ctx, f.org, f.olive, f.conn)
		Expect(errors.Is(err, githublink.ErrNoPublicURL)).To(BeTrue())
	})
})
