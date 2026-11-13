// SPDX-License-Identifier: Apache-2.0

package drill_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/test/drill"
)

// The OIDC checks of the drill (ADR 0109, amendment 2026-11-13, #983). The drill runs its own Dex (fakeidp.go) with
// three users. Most of the check is a Go HTTP flow through the install's real /auth/oidc/start and
// /auth/oidc/callback, so it runs on kind as well as on the lab; the lab adds one browser sign-in.

// oidcSourceRecords is what the source knew of each OIDC user before the backup, by username.
var oidcSourceRecords = map[string]drill.OIDCIdentity{}

// oidcLogin makes the flow's forwards for one install: its API on the fixed redirect port, and the provider on its
// fixed port. stop closes both.
func oidcLogin(ctx context.Context, s *side) (login drill.OIDCLogin, stop func()) {
	GinkgoHelper()
	bg := context.WithoutCancel(ctx)
	_, stopAPI, err := kube.PortForwardOn(bg, s.in.Namespace, "svc/"+s.name("api"), s.oidcPort, 8080)
	must(err, "port-forward the API of "+s.in.Release+" on the OIDC redirect port")
	_, stopIdP, err := kube.PortForwardOn(bg, top.FakesNamespace, "svc/"+drill.FakeIdPName, drill.FakeIdPForwardPort, drill.FakeIdPPort)
	if err != nil {
		stopAPI()
	}
	must(err, "port-forward the OIDC provider")
	login = drill.OIDCLogin{
		Base:        "http://127.0.0.1:" + strconv.Itoa(s.oidcPort),
		IdPHostPort: fmt.Sprintf("%s:%d", drill.FakeIdPHost(top.FakesNamespace), drill.FakeIdPPort),
		IdPAddr:     "127.0.0.1:" + strconv.Itoa(drill.FakeIdPForwardPort),
	}
	return login, func() {
		stopIdP()
		stopAPI()
	}
}

// oidcSignIn runs the flow for one user. A failure of the flow itself is retried a few times: the install's first
// call discovers the provider, and a forward can need a moment. A named Dex page fails each time the same way, so
// the retry costs seconds only.
func oidcSignIn(ctx context.Context, l drill.OIDCLogin, user drill.IdPUser) *drill.OIDCResult {
	GinkgoHelper()
	var res *drill.OIDCResult
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if res, err = l.SignIn(ctx, user.Email(), drill.FakeIdPPassword); err == nil {
			return res
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	must(err, "the OIDC flow for "+user.Username)
	return nil
}

func oidcIdentity(ctx context.Context, l drill.OIDCLogin, u drill.IdPUser) drill.OIDCIdentity {
	GinkgoHelper()
	res := oidcSignIn(ctx, l, u)
	Expect(res.Status).To(Equal(http.StatusFound), "the callback for %s answered %d: %s", u.Username, res.Status, res.Body)
	id, err := res.WhoAmI(ctx)
	must(err, "WhoAmI as "+u.Username)
	Expect(id.UserID).NotTo(BeEmpty(), "WhoAmI as %s names no user", u.Username)
	return id
}

// oidcUserCount is the number of users of the drill's provider in the install.
func oidcUserCount(ctx context.Context, s *side) int {
	GinkgoHelper()
	return sqlInt(ctx, s, fmt.Sprintf("SELECT count(*) FROM users WHERE oidc_issuer = '%s'", drill.FakeIdPIssuer(top.FakesNamespace)))
}

// oidcSource signs the three users in on the source, records their IDs and roles, then disables the disabled one in
// Koinon (the users row, as the install does for a removed user) and proves the callback refuses it.
func oidcSource(ctx context.Context, s *side) {
	GinkgoHelper()
	l, stop := oidcLogin(ctx, s)
	defer stop()
	for _, u := range drill.IdPUsers() {
		id := oidcIdentity(ctx, l, u)
		Expect(id.Roles["default"]).To(Equal(u.Role), "%s: the %s group maps to the %s role in the default org", u.Username, u.Group, u.Role)
		oidcSourceRecords[u.Username] = id
	}
	Expect(oidcUserCount(ctx, s)).To(Equal(len(drill.IdPUsers())), "one Koinon user for each provider user")
	Expect(oidcSourceRecords[drill.UserAdmin].UserID).NotTo(Equal(oidcSourceRecords[drill.UserMember].UserID))

	for _, u := range drill.IdPUsers() {
		if !u.Disabled {
			continue
		}
		id := oidcSourceRecords[u.Username].UserID
		out := sql(ctx, s, fmt.Sprintf("UPDATE users SET deleted_at = now() WHERE id = '%s' AND oidc_issuer = '%s' RETURNING id", id, drill.FakeIdPIssuer(top.FakesNamespace)))
		Expect(out).To(Equal(id), "disable %s in Koinon", u.Username)
		res := oidcSignIn(ctx, l, u)
		Expect(res.Status).To(Equal(http.StatusForbidden), "the source's callback for the disabled user %s answered %d: %s", u.Username, res.Status, res.Body)
	}
}

// oidcRow is a restored users row: whether it is disabled and the role of its group membership in the default org.
func oidcRow(ctx context.Context, s *side, userID string) (found, disabled bool, role string) {
	GinkgoHelper()
	out := sql(ctx, s, fmt.Sprintf(`SELECT (u.deleted_at IS NOT NULL)::text || '|' || coalesce((
  SELECT m.role::text FROM org_memberships m JOIN orgs o ON o.id = m.org_id
  WHERE m.user_id = u.id AND o.slug = 'default' AND m.source = 'idp_group' LIMIT 1), '')
FROM users u WHERE u.id = '%s' AND u.oidc_issuer = '%s'`, userID, drill.FakeIdPIssuer(top.FakesNamespace)))
	if out == "" {
		return false, false, ""
	}
	flag, role, _ := strings.Cut(out, "|")
	return true, flag == "true", role
}

// oidcTarget is the check `oidc-signin` on the restored copy.
func oidcTarget(ctx context.Context, s *side) {
	GinkgoHelper()
	Expect(oidcSourceRecords).To(HaveLen(len(drill.IdPUsers())), "the source recorded its OIDC users")

	// A login writes the group memberships again, so a role read after a sign-in proves the mapping, not the
	// restore. These rows are read before any sign-in on the copy: they are what the backup carried.
	By("the restored rows, before any sign-in on the copy")
	for _, u := range drill.IdPUsers() {
		rec := oidcSourceRecords[u.Username]
		found, disabled, role := oidcRow(ctx, s, rec.UserID)
		Expect(found).To(BeTrue(), "%s: the users row %s is in the restored database", u.Username, rec.UserID)
		Expect(disabled).To(Equal(u.Disabled), "%s: the restored row's disabled state", u.Username)
		Expect(role).To(Equal(rec.Roles["default"]), "%s: the restored group membership in the default org", u.Username)
	}
	Expect(oidcUserCount(ctx, s)).To(Equal(len(drill.IdPUsers())))

	By("the Go flow through the restored install")
	l, stop := oidcLogin(ctx, s)
	defer stop()
	for _, u := range drill.IdPUsers() {
		rec := oidcSourceRecords[u.Username]
		if u.Disabled {
			res := oidcSignIn(ctx, l, u)
			Expect(res.Status).To(Equal(http.StatusForbidden), "the callback for the disabled user %s answered %d: %s", u.Username, res.Status, res.Body)
			Expect(res.Body).To(ContainSubstring("disabled"))
			continue
		}
		id := oidcIdentity(ctx, l, u)
		Expect(id.UserID).To(Equal(rec.UserID), "%s is the same user after the restore", u.Username)
		Expect(id.Roles).To(Equal(rec.Roles), "%s keeps the role in every org after the restore", u.Username)
	}
	Expect(oidcUserCount(ctx, s)).To(Equal(len(drill.IdPUsers())), "no sign-in on the copy made a user")

	if top.BrowserSignin {
		By("one browser sign-in through Dex's login form (lab)")
		oidcBrowser(ctx, oidcSourceRecords[drill.UserAdmin], l)
	}
}

// oidcBrowser runs web/e2e-drill/oidc.spec.ts: the admin signs in on the copy through the Dex form, sees the Inbox,
// reloads and asks WhoAmI. The forwards are those of l, still open. Chromium maps the provider's cluster name to
// the forward itself (--host-resolver-rules, set from KOINON_DRILL_IDP_*).
func oidcBrowser(ctx context.Context, want drill.OIDCIdentity, l drill.OIDCLogin) {
	GinkgoHelper()
	logFile, err := os.Create(filepath.Join(outDir, "oidc-signin-browser.log"))
	must(err, "create the OIDC browser log")
	defer func() { _ = logFile.Close() }()
	cmd := exec.CommandContext(ctx, "pnpm", "--dir", filepath.Join(repoRoot, "web"), "run", "test:e2e-drill", "oidc.spec")
	// The password is the drill's fake one; it goes in the child's environment only.
	cmd.Env = append(os.Environ(),
		"KOINON_DRILL_WEB_URL="+l.Base,
		"KOINON_DRILL_IDP_HOST="+l.IdPHostPort,
		"KOINON_DRILL_IDP_ADDR="+l.IdPAddr,
		"KOINON_DRILL_OIDC_USER="+drill.IdPUsers()[0].Email(), // Dex asks for the e-mail address
		"KOINON_DRILL_OIDC_PASSWORD="+drill.FakeIdPPassword,
		"KOINON_DRILL_OIDC_EXPECT_USER_ID="+want.UserID,
		"KOINON_DRILL_OIDC_EXPECT_ROLE="+want.Roles["default"],
		"PLAYWRIGHT_OUTPUT_DIR="+filepath.Join(outDir, "oidc-signin-browser"),
	)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	must(cmd.Run(), "the Playwright OIDC spec on the restored copy (see oidc-signin-browser.log)")
}
