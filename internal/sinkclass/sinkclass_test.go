// SPDX-License-Identifier: Apache-2.0

package sinkclass_test

import (
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/sinkclass"
)

type hosts []string

func (h hosts) TrustsHost(host string) bool {
	for _, x := range h {
		if x == host {
			return true
		}
	}
	return false
}

var now = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

var _ = Describe("Declaration", func() {
	It("defaults to external-high, fixed, no address arguments and the server's legs", func() {
		d := sinkclass.Default()
		Expect(d.Class).To(Equal(sinkclass.ExternalHigh))
		Expect(d.Derivation).To(Equal(sinkclass.Fixed))
		Expect(d.AddressArgs).To(BeEmpty())
		Expect(d.ResultUntrustedInput).To(BeNil())
		Expect(d.ResultPrivateData).To(BeNil())
		Expect(d.IsDefault()).To(BeTrue())
		n, err := d.Normalize()
		Expect(err).NotTo(HaveOccurred())
		Expect(n.IsDefault()).To(BeTrue())
	})

	It("takes each result leg from the tool where it declares one, else from the server", func() {
		t, f := true, false
		u, p := sinkclass.Default().ResultLegs(true, false)
		Expect([]bool{u, p}).To(Equal([]bool{true, false}), "no declaration: the server's legs")
		u, p = sinkclass.Declaration{ResultUntrustedInput: &f}.ResultLegs(true, false)
		Expect([]bool{u, p}).To(Equal([]bool{false, false}), "one leg declared, the other the server's")
		u, p = sinkclass.Declaration{ResultPrivateData: &t}.ResultLegs(true, false)
		Expect([]bool{u, p}).To(Equal([]bool{true, true}))
	})

	It("is not the default once anything is declared", func() {
		t := true
		for _, d := range []sinkclass.Declaration{
			{Class: sinkclass.None, Derivation: sinkclass.Fixed},
			{Class: sinkclass.ExternalHigh, Derivation: sinkclass.Fixed, AddressArgs: []string{"owner"}},
			{Class: sinkclass.ExternalHigh, Derivation: sinkclass.Fetch, AddressArgs: []string{"url"}},
			{Class: sinkclass.ExternalHigh, Derivation: sinkclass.Fixed, ResultPrivateData: &t},
		} {
			Expect(d.IsDefault()).To(BeFalse(), "%+v", d)
		}
	})

	It("normalizes a derived declaration's class to external-high", func() {
		d, err := sinkclass.Declaration{Class: sinkclass.None, Derivation: sinkclass.CodeHost, AddressArgs: []string{"owner", "repo"}}.Normalize()
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Class).To(Equal(sinkclass.ExternalHigh))
	})

	DescribeTable("refuses what it cannot read",
		func(d sinkclass.Declaration) {
			_, err := d.Normalize()
			var inv *sinkclass.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue(), "%v", err)
		},
		Entry("an empty derivation", sinkclass.Declaration{Class: sinkclass.None}),
		Entry("an unknown derivation", sinkclass.Declaration{Class: sinkclass.None, Derivation: "search"}),
		Entry("an empty fixed class, which would silently classify the tool", sinkclass.Declaration{Derivation: sinkclass.Fixed}),
		Entry("an unknown class", sinkclass.Declaration{Class: "internal-ish", Derivation: sinkclass.Fixed}),
		Entry("code_host with one argument", sinkclass.Declaration{Derivation: sinkclass.CodeHost, AddressArgs: []string{"repo"}}),
		Entry("code_host with three", sinkclass.Declaration{Derivation: sinkclass.CodeHost, AddressArgs: []string{"a", "b", "c"}}),
		Entry("fetch with two", sinkclass.Declaration{Derivation: sinkclass.Fetch, AddressArgs: []string{"url", "u2"}}),
		Entry("fetch with none", sinkclass.Declaration{Derivation: sinkclass.Fetch}),
		Entry("an argument name with a space", sinkclass.Declaration{Class: sinkclass.None, Derivation: sinkclass.Fixed, AddressArgs: []string{"a b"}}),
		Entry("an empty argument name", sinkclass.Declaration{Class: sinkclass.None, Derivation: sinkclass.Fixed, AddressArgs: []string{""}}),
		Entry("the same argument twice", sinkclass.Declaration{Class: sinkclass.None, Derivation: sinkclass.Fixed, AddressArgs: []string{"x", "x"}}),
		Entry("code_host naming one argument for owner and repo", sinkclass.Declaration{Derivation: sinkclass.CodeHost, AddressArgs: []string{"repo", "repo"}}),
		Entry("nine address arguments", sinkclass.Declaration{Class: sinkclass.None, Derivation: sinkclass.Fixed, AddressArgs: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}}),
	)
})

var _ = Describe("Classify", func() {
	const gh, gitea = "conn-github", "conn-gitea"
	scope := sinkclass.NewScope([]sinkclass.Repo{
		{Connection: gh, Owner: "northwind", Name: "platform-handbook", Visibility: sinkclass.VisPrivate, CheckedAt: now.Add(-time.Minute)},
		{Connection: gh, Owner: "northwind", Name: "intranet", Visibility: sinkclass.VisInternal, CheckedAt: now.Add(-time.Minute)},
		{Connection: gh, Owner: "northwind", Name: "website", Visibility: sinkclass.VisPublic, CheckedAt: now.Add(-time.Minute)},
		{Connection: gh, Owner: "northwind", Name: "mystery", Visibility: sinkclass.VisUnknown, CheckedAt: now.Add(-time.Minute)},
		{Connection: gh, Owner: "northwind", Name: "old-reading", Visibility: sinkclass.VisPrivate, CheckedAt: now.Add(-2 * time.Hour)},
	}, 0)
	code := sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.CodeHost, AddressArgs: []string{"owner", "repo"}}
	fetch := sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.Fetch, AddressArgs: []string{"url"}}
	trusted := hosts{"docs.example.com"}
	repo := func(o, r string) map[string]string {
		return map[string]string{"owner": o, "repo": r, "title": "anything"}
	}

	Describe("a fixed tool", func() {
		It("has its declared class whatever the arguments", func() {
			for _, c := range sinkclass.Classes {
				Expect(sinkclass.Classify(sinkclass.Declaration{Class: c, Derivation: sinkclass.Fixed}, nil, gh, scope, trusted, now)).To(Equal(c))
			}
		})
		It("is external-high when the stored class is not one of the four", func() {
			Expect(sinkclass.Classify(sinkclass.Declaration{Class: "", Derivation: sinkclass.Fixed}, nil, gh, scope, trusted, now)).To(Equal(sinkclass.ExternalHigh))
		})
		It("is external-high for the default declaration", func() {
			Expect(sinkclass.Classify(sinkclass.Default(), nil, gh, scope, trusted, now)).To(Equal(sinkclass.ExternalHigh))
		})
	})

	Describe("a code-host tool", func() {
		DescribeTable("by repository",
			func(owner, name string, want sinkclass.Class) {
				Expect(sinkclass.Classify(code, repo(owner, name), gh, scope, trusted, now)).To(Equal(want))
			},
			Entry("a private repository in the scope is internal: the in-org write that must not count as a leak", "northwind", "platform-handbook", sinkclass.Internal),
			Entry("an internal-visibility repository is not public", "northwind", "intranet", sinkclass.Internal),
			Entry("names compare without case", "Northwind", "Platform-Handbook", sinkclass.Internal),
			Entry("a public repository is external-high", "northwind", "website", sinkclass.ExternalHigh),
			Entry("a repository of unknown visibility is external-high", "northwind", "mystery", sinkclass.ExternalHigh),
			Entry("a repository outside the scope is external-high", "northwind", "unlisted", sinkclass.ExternalHigh),
			Entry("a foreign owner is external-high", "attacker", "platform-handbook", sinkclass.ExternalHigh),
			Entry("a reading older than the window counts as unknown", "northwind", "old-reading", sinkclass.ExternalHigh),
			Entry("owner and repo glued into the owner argument", "northwind/platform-handbook", "", sinkclass.ExternalHigh),
			Entry("a path in the repo argument", "northwind", "platform-handbook/../website", sinkclass.ExternalHigh),
			Entry("a dot-dot repo", "northwind", "..", sinkclass.ExternalHigh),
			Entry("an empty repo", "northwind", "", sinkclass.ExternalHigh),
			Entry("an empty owner", "", "platform-handbook", sinkclass.ExternalHigh),
			Entry("a space in the repo", "northwind", "platform handbook", sinkclass.ExternalHigh),
		)

		It("is external-high when an address argument is missing from the call", func() {
			Expect(sinkclass.Classify(code, map[string]string{"owner": "northwind"}, gh, scope, trusted, now)).To(Equal(sinkclass.ExternalHigh))
			Expect(sinkclass.Classify(code, nil, gh, scope, trusted, now)).To(Equal(sinkclass.ExternalHigh))
		})

		It("reads the arguments the declaration names, not the ones called owner and repo", func() {
			d := sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.CodeHost, AddressArgs: []string{"org", "project"}}
			Expect(sinkclass.Classify(d, map[string]string{"org": "northwind", "project": "platform-handbook"}, gh, scope, trusted, now)).To(Equal(sinkclass.Internal))
			Expect(sinkclass.Classify(d, repo("northwind", "platform-handbook"), gh, scope, trusted, now)).To(Equal(sinkclass.ExternalHigh))
		})

		It("is never internal for an empty scope", func() {
			Expect(sinkclass.Classify(code, repo("northwind", "platform-handbook"), gh, sinkclass.NewScope(nil, 0), trusted, now)).To(Equal(sinkclass.ExternalHigh))
		})

		It("is not changed by the payload", func() {
			args := repo("northwind", "platform-handbook")
			args["body"] = "https://attacker.example/?q=secret"
			Expect(sinkclass.Classify(code, args, gh, scope, trusted, now)).To(Equal(sinkclass.Internal))
		})

		It("looks the repository up in the linked connection only", func() {
			s := sinkclass.NewScope([]sinkclass.Repo{
				{Connection: gitea, Owner: "northwind", Name: "platform-handbook", Visibility: sinkclass.VisPrivate, CheckedAt: now},
				{Connection: gh, Owner: "northwind", Name: "platform-handbook", Visibility: sinkclass.VisPublic, CheckedAt: now},
			}, 0)
			Expect(sinkclass.Classify(code, repo("northwind", "platform-handbook"), gitea, s, trusted, now)).To(Equal(sinkclass.Internal), "the connection that lists it private")
			Expect(sinkclass.Classify(code, repo("northwind", "platform-handbook"), gh, s, trusted, now)).To(Equal(sinkclass.ExternalHigh), "the same name on a public repository of another host")
		})

		It("is external-high for a repository only another connection lists as private", func() {
			s := sinkclass.NewScope([]sinkclass.Repo{{Connection: gitea, Owner: "northwind", Name: "platform-handbook", Visibility: sinkclass.VisPrivate, CheckedAt: now}}, 0)
			Expect(sinkclass.Classify(code, repo("northwind", "platform-handbook"), gh, s, trusted, now)).To(Equal(sinkclass.ExternalHigh))
			Expect(sinkclass.Classify(code, repo("northwind", "platform-handbook"), gitea, s, trusted, now)).To(Equal(sinkclass.Internal))
		})

		It("is external-high when the server is linked to no connection", func() {
			Expect(sinkclass.Classify(code, repo("northwind", "platform-handbook"), "", scope, trusted, now)).To(Equal(sinkclass.ExternalHigh))
		})

		It("is external-high for a reading dated in the future, beyond the clock skew", func() {
			s := sinkclass.NewScope([]sinkclass.Repo{
				{Connection: gh, Owner: "n", Name: "future", Visibility: sinkclass.VisPrivate, CheckedAt: now.Add(24 * time.Hour)},
				{Connection: gh, Owner: "n", Name: "skewed", Visibility: sinkclass.VisPrivate, CheckedAt: now.Add(sinkclass.MaxClockSkew / 2)},
			}, 0)
			Expect(sinkclass.Classify(code, repo("n", "future"), gh, s, trusted, now)).To(Equal(sinkclass.ExternalHigh))
			Expect(sinkclass.Classify(code, repo("n", "skewed"), gh, s, trusted, now)).To(Equal(sinkclass.Internal), "a small skew is tolerated")
		})

		It("is external-high for a non-ASCII name, also one that lower-cases to an ASCII letter", func() {
			s := sinkclass.NewScope([]sinkclass.Repo{{Connection: gh, Owner: "kfoo", Name: "r", Visibility: sinkclass.VisPrivate, CheckedAt: now}}, 0)
			Expect(sinkclass.Classify(code, repo("kfoo", "r"), gh, s, trusted, now)).To(Equal(sinkclass.Internal))
			Expect(sinkclass.Classify(code, repo("\u212Afoo", "r"), gh, s, trusted, now)).To(Equal(sinkclass.ExternalHigh), "U+212A")
			Expect(sinkclass.Classify(code, repo("kfoo", "r\u00e9"), gh, s, trusted, now)).To(Equal(sinkclass.ExternalHigh))
			Expect(sinkclass.Classify(code, repo("kfoo", "r\x00"), gh, s, trusted, now)).To(Equal(sinkclass.ExternalHigh))
		})

		It("honours the freshness window it was built with", func() {
			s := sinkclass.NewScope([]sinkclass.Repo{{Connection: gh, Owner: "n", Name: "r", Visibility: sinkclass.VisPrivate, CheckedAt: now.Add(-10 * time.Minute)}}, 5*time.Minute)
			Expect(sinkclass.Classify(code, repo("n", "r"), gh, s, trusted, now)).To(Equal(sinkclass.ExternalHigh))
			Expect(sinkclass.Classify(code, repo("n", "r"), gh, s, trusted, now.Add(-8*time.Minute))).To(Equal(sinkclass.Internal))
		})
	})

	Describe("a fetch tool", func() {
		call := func(u string) map[string]string { return map[string]string{"url": u} }
		DescribeTable("by host",
			func(u string, want sinkclass.Class) {
				Expect(sinkclass.Classify(fetch, call(u), gh, scope, trusted, now)).To(Equal(want))
			},
			Entry("a trusted host is internal", "https://docs.example.com/guide", sinkclass.Internal),
			Entry("the path, query and fragment are payload, not address", "https://docs.example.com/a?q=secret#frag", sinkclass.Internal),
			Entry("another host is external-low", "https://example.org/", sinkclass.ExternalLow),
			Entry("a subdomain of a trusted exact host is external-low", "https://a.docs.example.com/", sinkclass.ExternalLow),
			Entry("a lookalike prefix is external-low", "https://docs.example.com.evil.net/", sinkclass.ExternalLow),
			Entry("userinfo does not move the host", "https://docs.example.com@evil.net/", sinkclass.ExternalLow),
			Entry("a port does not change the host", "https://docs.example.com:8443/", sinkclass.Internal),
			Entry("http is accepted", "http://example.org/", sinkclass.ExternalLow),
			Entry("a host in capitals", "https://DOCS.EXAMPLE.COM/", sinkclass.Internal),
			Entry("an IP address host is external-low", "https://127.0.0.1/", sinkclass.ExternalLow),
			Entry("a URL without a host is external-high", "https:///path", sinkclass.ExternalHigh),
			Entry("text that is not a URL is external-high", "not a url", sinkclass.ExternalHigh),
			Entry("a relative URL is external-high", "/docs/guide", sinkclass.ExternalHigh),
			Entry("a host without a scheme is external-high", "docs.example.com/guide", sinkclass.ExternalHigh),
			Entry("another scheme is external-high", "ftp://docs.example.com/", sinkclass.ExternalHigh),
			Entry("a file URL is external-high", "file:///etc/passwd", sinkclass.ExternalHigh),
			Entry("an empty URL is external-high", "", sinkclass.ExternalHigh),
			Entry("a URL that does not parse is external-high", "https://exa mple.com/", sinkclass.ExternalHigh),
		)

		It("is external-high when the url argument is missing", func() {
			Expect(sinkclass.Classify(fetch, map[string]string{"q": "x"}, gh, scope, trusted, now)).To(Equal(sinkclass.ExternalHigh))
		})

		It("is external-low for every host when the org trusts none", func() {
			Expect(sinkclass.Classify(fetch, call("https://docs.example.com/"), gh, scope, nil, now)).To(Equal(sinkclass.ExternalLow))
		})
	})

	It("is external-high for a derived declaration with the wrong number of arguments", func() {
		bad := sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.CodeHost, AddressArgs: []string{"owner"}}
		Expect(sinkclass.Classify(bad, repo("northwind", "platform-handbook"), gh, scope, trusted, now)).To(Equal(sinkclass.ExternalHigh))
		bad = sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.Fetch}
		Expect(sinkclass.Classify(bad, map[string]string{"url": "https://docs.example.com/"}, gh, scope, trusted, now)).To(Equal(sinkclass.ExternalHigh))
	})

	It("is external-high for a derivation it does not know", func() {
		Expect(sinkclass.Classify(sinkclass.Declaration{Class: sinkclass.None, Derivation: "oracle"}, nil, gh, scope, trusted, now)).To(Equal(sinkclass.ExternalHigh))
	})
})
