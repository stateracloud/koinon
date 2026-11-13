// SPDX-License-Identifier: Apache-2.0

package drill_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/test/drill"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "dex", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The shared fixtures are the pages Dex v2.46.1 renders (web/src/lib/drill-dex.test.ts reads the same files).
func TestClassifyDexPageNamesEachPageDexCanShow(t *testing.T) {
	for file, want := range map[string]struct {
		kind    drill.DexPageKind
		errPart string // the named error, "" when the page is expected
	}{
		"login.html":          {drill.DexLoginForm, ""},
		"wrong-password.html": {drill.DexWrongPassword, "dex refused the password for drill-admin"},
		"approval.html":       {drill.DexApproval, "dex asks for approval: skipApprovalScreen is off"},
		"chooser.html":        {drill.DexChooser, "dex shows a connector chooser"},
		"mfa-totp.html":       {drill.DexMFA, "dex asks for a second factor"},
		"error.html":          {drill.DexError, `dex error: Bad Request: Unregistered redirect_uri ("http://127.0.0.1:9/auth/oidc/callback").`},
	} {
		p := drill.ClassifyDexPage(fixture(t, file))
		if p.Kind != want.kind {
			t.Errorf("%s: kind %q, want %q", file, p.Kind, want.kind)
		}
		err := p.Err("drill-admin")
		switch {
		case want.errPart == "" && err != nil:
			t.Errorf("%s: unexpected error %v", file, err)
		case want.errPart != "" && (err == nil || !strings.Contains(err.Error(), want.errPart)):
			t.Errorf("%s: error %v, want it to contain %q", file, err, want.errPart)
		}
	}
	if p := drill.ClassifyDexPage("<html><body><h1>hello</h1></body></html>"); p.Kind != drill.DexUnknown || p.Err("x") == nil {
		t.Errorf("a page that is not Dex's must be unknown and an error: %+v", p)
	}
}

// world is a Koinon stand-in and a Dex stand-in on two listeners. The Dex one is reached by a name with no DNS
// (dex.test:5556), as in the drill.
type world struct {
	koinon, dex *httptest.Server
	idpHostPort string
	// page, when set, replaces the login form with a fixture.
	page string
	// callbackStatus is what the callback answers.
	callbackStatus  int
	sawSecureCookie bool
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{idpHostPort: "dex.test:5556", callbackStatus: http.StatusFound}
	w.koinon = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/oidc/start":
			http.SetCookie(rw, &http.Cookie{Name: "state", Value: "s1", Path: "/auth/oidc", Secure: true})
			http.Redirect(rw, r, "http://"+w.idpHostPort+"/dex/auth?client_id=koinon-drill&state=s1", http.StatusFound)
		case "/auth/oidc/callback":
			if c, err := r.Cookie("state"); err != nil || c.Value != r.URL.Query().Get("state") {
				http.Error(rw, "login state mismatch", http.StatusBadRequest)
				return
			}
			if w.callbackStatus != http.StatusFound {
				http.Error(rw, "this account is disabled", w.callbackStatus)
				return
			}
			http.SetCookie(rw, &http.Cookie{Name: "__Host-session", Value: "tok", Path: "/", Secure: true})
			http.Redirect(rw, r, "/", http.StatusFound)
		case "/koinon.auth.v1alpha1.AuthService/WhoAmI":
			if c, err := r.Cookie("__Host-session"); err != nil || c.Value != "tok" {
				http.Error(rw, "no session", http.StatusUnauthorized)
				return
			}
			w.sawSecureCookie = true
			_, _ = fmt.Fprint(rw, `{"user":{"id":"u-1"},"memberships":[{"orgSlug":"default","role":"ORG_ROLE_ADMIN"},{"orgSlug":"personal","role":"ORG_ROLE_OWNER"}]}`)
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(w.koinon.Close)
	w.dex = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Host != w.idpHostPort {
			http.Error(rw, "wrong host "+r.Host, http.StatusMisdirectedRequest)
			return
		}
		switch {
		case r.URL.Path == "/dex/auth":
			http.Redirect(rw, r, "/dex/auth/local/login?back=&state="+r.URL.Query().Get("state"), http.StatusFound)
		case r.URL.Path == "/dex/auth/local/login" && r.Method == http.MethodGet:
			page := w.page
			if page == "" {
				page = "login.html"
			}
			_, _ = fmt.Fprint(rw, fixtureNoT(page))
		case r.URL.Path == "/dex/auth/local/login" && r.Method == http.MethodPost:
			_ = r.ParseForm()
			if r.PostForm.Get("login") != "drill-admin" || r.PostForm.Get("password") != drill.FakeIdPPassword {
				rw.WriteHeader(http.StatusUnauthorized) // as Dex does
				_, _ = fmt.Fprint(rw, fixtureNoT("wrong-password.html"))
				return
			}
			q := url.Values{"code": {"c1"}, "state": {"s1"}}
			http.Redirect(rw, r, w.koinon.URL+"/auth/oidc/callback?"+q.Encode(), http.StatusSeeOther)
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(w.dex.Close)
	return w
}

func fixtureNoT(name string) string {
	b, err := os.ReadFile(filepath.Join("testdata", "dex", name))
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (w *world) login() drill.OIDCLogin {
	return drill.OIDCLogin{Base: w.koinon.URL, IdPHostPort: w.idpHostPort, IdPAddr: strings.TrimPrefix(w.dex.URL, "http://")}
}

func TestTheGoFlowSignsInThroughStartFormAndCallback(t *testing.T) {
	w := newWorld(t)
	res, err := w.login().SignIn(context.Background(), "drill-admin", drill.FakeIdPPassword)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Redirected() {
		t.Fatalf("callback answered %d: %s", res.Status, res.Body)
	}
	id, err := res.WhoAmI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.UserID != "u-1" || id.Roles["default"] != "admin" || id.Roles["personal"] != "owner" {
		t.Errorf("identity %+v", id)
	}
	if !w.sawSecureCookie {
		t.Error("the Secure session cookie was not sent back over http")
	}
}

func TestTheGoFlowReturnsTheCallbacksRefusalAsAnAnswer(t *testing.T) {
	w := newWorld(t)
	w.callbackStatus = http.StatusForbidden
	res, err := w.login().SignIn(context.Background(), "drill-admin", drill.FakeIdPPassword)
	if err != nil {
		t.Fatalf("a 403 from the callback is an answer, not a flow error: %v", err)
	}
	if res.Status != http.StatusForbidden || !strings.Contains(res.Body, "disabled") {
		t.Errorf("status %d body %q", res.Status, res.Body)
	}
}

func TestTheGoFlowNamesTheDexPageItMet(t *testing.T) {
	for page, want := range map[string]string{
		"approval.html": "dex asks for approval",
		"chooser.html":  "connector chooser",
		"error.html":    "dex error: Bad Request",
		"mfa-totp.html": "second factor",
	} {
		w := newWorld(t)
		w.page = page
		_, err := w.login().SignIn(context.Background(), "drill-admin", drill.FakeIdPPassword)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", page, err, want)
		}
	}
	w := newWorld(t)
	_, err := w.login().SignIn(context.Background(), "drill-admin", "koinon_test_not-the-password")
	if err == nil || !strings.Contains(err.Error(), "dex refused the password for drill-admin") {
		t.Errorf("wrong password: %v", err)
	}
	if strings.Contains(fmt.Sprint(err), "state=") {
		t.Errorf("an error must not carry a flow's state: %v", err)
	}
}
