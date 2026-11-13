// SPDX-License-Identifier: Apache-2.0

package drill

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DexPageKind is what a page of the drill's provider shows. The Go flow and the browser step (web/src/lib/drill-dex.ts,
// the same rules over the same fixtures in testdata/dex) both name the page they got instead of waiting for a
// heading that never comes.
type DexPageKind string

// The pages Dex can show a client of the drill (templates of Dex v2.46.1, web/templates).
const (
	DexLoginForm     DexPageKind = "login-form"     // password.html, no error: expected
	DexWrongPassword DexPageKind = "wrong-password" // password.html with #login-error
	DexApproval      DexPageKind = "approval"       // approval.html: skipApprovalScreen is off, or prompt=consent
	DexChooser       DexPageKind = "chooser"        // login.html: more than one connector
	DexMFA           DexPageKind = "mfa"            // totp_verify.html, webauthn_verify.html
	DexError         DexPageKind = "error"          // error.html: bad redirect URI, unknown client, PKCE missing
	DexUnknown       DexPageKind = "unknown"
)

// DexPage is a classified page.
type DexPage struct {
	Kind    DexPageKind
	Heading string // the theme-heading, for an error or an unknown page
	Message string // the paragraph under it, for an error page
}

var (
	dexHeadingRE = regexp.MustCompile(`(?s)<h2 class="theme-heading">(.*?)</h2>`)
	dexMessageRE = regexp.MustCompile(`(?s)</h2>\s*<p>(.*?)</p>`)
	dexFormRE    = regexp.MustCompile(`(?s)<form[^>]*\saction="([^"]*)"`)
	dexTagRE     = regexp.MustCompile(`<[^>]*>`)
)

// ClassifyDexPage tells which Dex page an HTML body is. The order matters: the approval and the MFA pages carry
// buttons and error boxes that look like the chooser and the wrong-password page.
func ClassifyDexPage(body string) DexPage {
	text := func(s string) string { return strings.TrimSpace(html.UnescapeString(dexTagRE.ReplaceAllString(s, ""))) }
	p := DexPage{Kind: DexUnknown}
	if m := dexHeadingRE.FindStringSubmatch(body); m != nil {
		p.Heading = text(m[1])
	}
	if m := dexMessageRE.FindStringSubmatch(body); m != nil {
		p.Message = text(m[1])
	}
	switch {
	case strings.Contains(body, `id="webauthn-btn"`) || strings.Contains(body, `id="totp"`):
		p.Kind = DexMFA
	case p.Heading == "Grant Access":
		p.Kind = DexApproval
	case strings.Contains(body, `id="login"`) && strings.Contains(body, `id="password"`):
		p.Kind = DexLoginForm
		if strings.Contains(body, `id="login-error"`) {
			p.Kind = DexWrongPassword
		}
	case strings.Contains(body, "theme-btn-provider"):
		p.Kind = DexChooser
	case p.Heading != "" && p.Message != "":
		p.Kind = DexError
	}
	return p
}

// Err is the named error for a page the flow must not meet, or nil for the login form. user is the one signing in.
func (p DexPage) Err(user string) error {
	switch p.Kind {
	case DexLoginForm:
		return nil
	case DexWrongPassword:
		return fmt.Errorf("dex refused the password for %s", user)
	case DexApproval:
		return fmt.Errorf("dex asks for approval: skipApprovalScreen is off, or the request forces it")
	case DexChooser:
		return fmt.Errorf("dex shows a connector chooser: its config has more than one connector")
	case DexMFA:
		return fmt.Errorf("dex asks for a second factor: the drill's config must not set one")
	case DexError:
		return fmt.Errorf("dex error: %s: %s", p.Heading, p.Message)
	}
	return fmt.Errorf("dex shows a page the drill does not know (heading %q)", p.Heading)
}

// OIDCLogin signs a user in through an install's real /auth/oidc/start and /auth/oidc/callback and the drill's
// Dex, over plain HTTP with a cookie jar. It follows each redirect itself, so a failure names the hop.
type OIDCLogin struct {
	// Base is the install's API as the host reaches it (the redirect URL's origin: http://127.0.0.1:18181).
	Base string
	// IdPHostPort is the issuer's host and port as written in the issuer URL (dex.<ns>.svc.cluster.local:5556).
	IdPHostPort string
	// IdPAddr is where the host's port-forward to the provider listens (127.0.0.1:18556). The client dials it
	// whenever a URL names IdPHostPort, which is how the host reaches a cluster name with no DNS for it.
	IdPAddr string
}

// OIDCResult is what the callback answered, and the session it made when it redirected.
type OIDCResult struct {
	// Status and Body are the callback's own answer: 302 with a session cookie on success, 403 for a disabled
	// user, 503 while the copy is held.
	Status int
	Body   string
	client *http.Client
	base   string
}

// Redirected says whether the callback signed the user in.
func (r *OIDCResult) Redirected() bool { return r.Status == http.StatusFound }

// OIDCIdentity is who a session is: the user and the role in each org by slug.
type OIDCIdentity struct {
	UserID string
	Roles  map[string]string
}

// looseJar keeps every cookie it is given for a host and returns them all, whatever their Secure flag or path. The
// install sets Secure cookies (its public URL is https by default) and a browser accepts them on http://127.0.0.1,
// but the standard jar would not send them back.
type looseJar struct {
	mu sync.Mutex
	m  map[string]map[string]*http.Cookie
}

func (j *looseJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.m == nil {
		j.m = map[string]map[string]*http.Cookie{}
	}
	if j.m[u.Host] == nil {
		j.m[u.Host] = map[string]*http.Cookie{}
	}
	for _, c := range cookies {
		if c.MaxAge < 0 || (!c.Expires.IsZero() && c.Expires.Before(time.Now())) {
			delete(j.m[u.Host], c.Name)
			continue
		}
		j.m[u.Host][c.Name] = c
	}
}

func (j *looseJar) Cookies(u *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []*http.Cookie
	for _, c := range j.m[u.Host] {
		out = append(out, c)
	}
	return out
}

func (l OIDCLogin) client() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{
		Timeout: 30 * time.Second,
		Jar:     &looseJar{},
		// Every redirect is followed by SignIn, hop by hop.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy: nil, // a proxy variable of the environment must not catch a loopback or cluster name
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if addr == l.IdPHostPort {
					addr = l.IdPAddr
				}
				return dialer.DialContext(ctx, network, addr)
			},
		},
	}
}

const maxHops = 20

// SignIn runs the authorization code flow for one user. login is what Dex's form asks for: the e-mail address
// (IdPUser.Email), not the username. An error is a failure of the flow before the callback (a
// named Dex page, a bad redirect, an unreachable host); what the callback itself answers is in the result, whatever
// its status, so the caller can expect a 403.
func (l OIDCLogin) SignIn(ctx context.Context, login, password string) (*OIDCResult, error) {
	baseURL, err := url.Parse(l.Base)
	if err != nil {
		return nil, err
	}
	c := l.client()
	next, method, form := l.Base+"/auth/oidc/start?return_to=%2F", http.MethodGet, url.Values(nil)
	submitted := false
	for hop := 0; hop < maxHops; hop++ {
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, err := http.NewRequestWithContext(ctx, method, next, body)
		if err != nil {
			return nil, err
		}
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", method, redact(req.URL), err)
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		here := req.URL
		if here.Host == baseURL.Host && here.Path == "/auth/oidc/callback" {
			return &OIDCResult{Status: resp.StatusCode, Body: strings.TrimSpace(string(b)), client: c, base: l.Base}, nil
		}
		switch {
		case resp.StatusCode >= 300 && resp.StatusCode < 400:
			loc, err := here.Parse(resp.Header.Get("Location"))
			if err != nil {
				return nil, fmt.Errorf("%s answered a bad Location: %w", redact(here), err)
			}
			if loc.Host != baseURL.Host && loc.Host != l.IdPHostPort {
				return nil, fmt.Errorf("%s redirected to %s, which is neither the install nor the provider", redact(here), loc.Host)
			}
			next, method, form = loc.String(), http.MethodGet, nil
		case here.Host == l.IdPHostPort:
			// A page of Dex, whatever its status: the login form is 200, a wrong password 401, an error page 400.
			page := ClassifyDexPage(string(b))
			if err := page.Err(login); err != nil {
				return nil, err
			}
			if submitted {
				return nil, fmt.Errorf("dex shows the login form again after the password was sent")
			}
			m := dexFormRE.FindStringSubmatch(string(b))
			if m == nil {
				return nil, fmt.Errorf("dex login page has no form")
			}
			action, err := here.Parse(html.UnescapeString(m[1]))
			if err != nil {
				return nil, err
			}
			next, method, form, submitted = action.String(), http.MethodPost, url.Values{"login": {login}, "password": {password}}, true
		default:
			return nil, fmt.Errorf("%s answered %d: %s", redact(here), resp.StatusCode, snippet(b))
		}
	}
	return nil, fmt.Errorf("more than %d redirects", maxHops)
}

// WhoAmI asks the install who the session is (the web UI's own call).
func (r *OIDCResult) WhoAmI(ctx context.Context) (OIDCIdentity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+"/koinon.auth.v1alpha1.AuthService/WhoAmI", strings.NewReader("{}"))
	if err != nil {
		return OIDCIdentity{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	resp, err := r.client.Do(req)
	if err != nil {
		return OIDCIdentity{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return OIDCIdentity{}, fmt.Errorf("WhoAmI answered %d: %s", resp.StatusCode, snippet(b))
	}
	var me struct {
		User        struct{ ID string } `json:"user"`
		Memberships []struct {
			OrgSlug string `json:"orgSlug"`
			Role    string `json:"role"`
		} `json:"memberships"`
	}
	if err := json.Unmarshal(b, &me); err != nil {
		return OIDCIdentity{}, fmt.Errorf("decode WhoAmI: %w", err)
	}
	id := OIDCIdentity{UserID: me.User.ID, Roles: map[string]string{}}
	for _, m := range me.Memberships {
		id.Roles[m.OrgSlug] = NormalizeRole(m.Role)
	}
	return id, nil
}

// NormalizeRole turns the API's enum name (ORG_ROLE_ADMIN) into the database's (admin).
func NormalizeRole(r string) string { return strings.ToLower(strings.TrimPrefix(r, "ORG_ROLE_")) }

// redact is a URL without its query: the state and the code of a flow are not for a log.
func redact(u *url.URL) string { return u.Scheme + "://" + u.Host + u.Path }

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
