// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
)

// The manifest flow's client side (issue #1175).
var _ = Describe("App manifest", func() {
	var (
		fake *githubtest.Fake
		host *github.Host
		site codehost.Connection
	)
	BeforeEach(func() {
		fake = githubtest.New()
		DeferCleanup(fake.Server.Close)
		host = github.NewHost(github.NewClient(github.Options{BaseURL: fake.URL()}))
		site = codehost.Connection{ID: "c1", Connector: github.HostID, Config: []byte(`{}`)}
	})

	It("asks for the setup guide's permissions and nothing it forbids", func() {
		t, ok := connectors.Get(github.HostID)
		Expect(ok).To(BeTrue())
		m := github.NewManifest(t, "Acme", github.ManifestURLs{
			Home: "https://k.example.test", Created: "https://k.example.test/created",
			Installed: "https://k.example.test/installed", Callback: "https://k.example.test/cb",
		})
		Expect(m.DefaultPermissions).To(Equal(t.Spec.Auth.Permissions))
		for _, p := range t.Spec.Auth.ForbiddenPermissions {
			Expect(m.DefaultPermissions).NotTo(HaveKey(p))
		}
		Expect(m.Public).To(BeFalse())
		Expect(m.HookAttributes.Active).To(BeFalse())
		js, err := json.Marshal(m)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(js)).To(ContainSubstring(`"default_events":[]`))
	})

	It("trades a code once and never prints the App's secrets", func(ctx context.Context) {
		post, err := host.ManifestPostURL(site, "", "koinon-test-state")
		Expect(err).NotTo(HaveOccurred())
		Expect(post).To(Equal(fake.WebURL() + "/settings/apps/new?state=koinon-test-state"))
		m, _ := json.Marshal(github.Manifest{Name: "Acme", URL: "https://k.example.test", RedirectURL: "https://k.example.test/created"})
		_, err = fake.CreateFromManifest(post, string(m), "koinon-test-code")
		Expect(err).NotTo(HaveOccurred())

		app, err := host.ConvertManifest(ctx, site, "koinon-test-code")
		Expect(err).NotTo(HaveOccurred())
		Expect(app.ID).To(Equal(fake.AppID))
		Expect(app.Slug).To(Equal(githubtest.FakeAppSlug))
		Expect(app.ClientID).To(Equal(githubtest.FakeClientID))
		Expect(string(app.ClientSecret)).To(Equal(githubtest.FakeClientSecret))
		Expect(app.PEM).To(Equal(fake.PEM))

		var log strings.Builder
		slog.New(slog.NewTextHandler(&log, nil)).Info("app", "app", app)
		for _, s := range []string{fmt.Sprint(app), fmt.Sprintf("%#v", app), fmt.Sprintf("%+v", app), log.String()} {
			Expect(s).NotTo(ContainSubstring(githubtest.FakeClientSecret))
			Expect(s).NotTo(ContainSubstring("PRIVATE KEY"))
		}

		_, err = host.ConvertManifest(ctx, site, "koinon-test-code")
		Expect(err).To(HaveOccurred(), "a code works once")
		_, err = host.ConvertManifest(ctx, site, "../x")
		Expect(err).To(HaveOccurred())

		install, err := host.InstallURL(site, app.Slug, "koinon-test-state")
		Expect(err).NotTo(HaveOccurred())
		Expect(install).To(Equal(fake.WebURL() + "/apps/" + githubtest.FakeAppSlug + "/installations/new?state=koinon-test-state"))
	})

	It("knows the App's installation from another one", func(ctx context.Context) {
		Expect(host.CheckInstallation(ctx, site, fake.AppID, fake.InstallationID, fake.PEM)).To(Succeed())
		Expect(host.CheckInstallation(ctx, site, fake.AppID, fake.InstallationID+1, fake.PEM)).To(MatchError(github.ErrNotTheAppsInstallation))
	})
})
