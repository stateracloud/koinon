// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/cli"
)

var _ = Describe("koinon-api's startup", func() {
	const db = "postgres://koinon_test:not-a-real-password@localhost/koinon"
	start := func(env map[string]string, args ...string) error {
		return cli.Check(NewRootCommand(), args, env)
	}

	It("runs the migrate subcommand without the OTLP token, as the chart's migrate Job does", func() {
		Expect(start(map[string]string{"KOINON_DATABASE_URL": db}, "migrate", "--wait", "2m", "--durable")).To(Succeed())
	})

	It("runs the grant-only migrate of the chart's post-upgrade hook, without --durable", func() {
		Expect(start(map[string]string{"KOINON_DATABASE_URL": db}, "migrate", "--grants-only", "--wait", "5m")).To(Succeed())
		Expect(start(map[string]string{"KOINON_DATABASE_URL": db}, "migrate", "--grants-only", "--durable")).
			To(MatchError(ContainSubstring("--grants-only and --durable are exclusive")))
	})

	It("takes the database URL from the flag, KOINON_DATABASE_URL or DATABASE_URL", func() {
		Expect(start(nil, "migrate", "--database-url="+db)).To(Succeed())
		Expect(start(map[string]string{"KOINON_DATABASE_URL": db}, "migrate")).To(Succeed())
		Expect(start(map[string]string{"DATABASE_URL": db}, "migrate")).To(Succeed())
		Expect(start(nil, "migrate")).To(MatchError(ContainSubstring(`"database-url" not set`)))
	})

	It("refuses to serve the OTLP receiver without its token", func() {
		Expect(start(map[string]string{"KOINON_DATABASE_URL": db})).To(MatchError(ContainSubstring("--otlp-token-dir is required")))
		Expect(start(map[string]string{"KOINON_DATABASE_URL": db}, "serve")).To(MatchError(ContainSubstring("--otlp-token-dir is required")))
	})

	It("serves without the receiver when it is turned off, by flag or by an empty variable", func() {
		Expect(start(nil, "--database-url="+db, "--otlp-addr=")).To(Succeed())
		Expect(start(map[string]string{"KOINON_DATABASE_URL": db, "KOINON_OTLP_ADDR": ""})).To(Succeed())
	})

	It("serves when a bare flag list is given, as the pods did before serve was a subcommand", func() {
		Expect(start(map[string]string{"KOINON_DATABASE_URL": db, "KOINON_OTLP_TOKEN_DIR": "/otlp"}, "--addr", ":8080")).To(Succeed())
	})

	It("checks the rules between flags on serve only", func() {
		env := map[string]string{"KOINON_DATABASE_URL": db, "KOINON_OTLP_ADDR": "", "KOINON_DEFAULT_ORG_GROUP_MAPPINGS": "ops=admin"}
		Expect(start(env, "serve")).To(MatchError(ContainSubstring("--default-org-group-mappings needs --oidc-issuer-url")))
		Expect(start(map[string]string{"KOINON_DATABASE_URL": db}, "migrate")).To(Succeed())
		Expect(start(env, "migrate")).To(MatchError(ContainSubstring("koinon-api migrate reads no environment variable KOINON_DEFAULT_ORG_GROUP_MAPPINGS")))
		Expect(start(map[string]string{"KOINON_DATABASE_URL": db, "KOINON_OTLP_ADDR": "", "KOINON_OIDC_PLATFORM_ADMIN_GROUPS": "ops"})).
			To(MatchError(ContainSubstring("--oidc-platform-admin-groups needs --oidc-issuer-url")))
	})

	It("names the variable whose value does not parse", func() {
		err := start(map[string]string{"KOINON_DATABASE_URL": db, "KOINON_WAIT": "soon"}, "migrate")
		Expect(err).To(MatchError(ContainSubstring("KOINON_WAIT")))
	})

	It("refuses an unknown flag, an unknown command and an environment variable nothing reads", func() {
		Expect(start(nil, "migrate", "--database-url="+db, "--otlp-addr=")).To(MatchError(ContainSubstring("unknown flag")))
		Expect(start(nil, "frobnicate")).To(MatchError(ContainSubstring("unknown command")))
		Expect(start(map[string]string{"KOINON_DATABASE_URL": db, "KOINON_OTLP_TOKEN_DIR": "/x", "KOINON_NO_SUCH": "1"})).
			To(MatchError(ContainSubstring("KOINON_NO_SUCH")))
	})

	It("keeps the preflight Job's flags and POD_NAMESPACE", func() {
		Expect(start(map[string]string{"POD_NAMESPACE": "koinon"}, "preflight", "--postgres-mode=external", "--gateway-mode=kubernetes",
			"--probe-image=example.test/koinon-api:test")).To(Succeed())
		Expect(start(nil, "preflight", "--postgres-mode=mysql")).To(MatchError(ContainSubstring("postgres mode")))
	})

	It("needs the ensure-secrets Job's two inputs", func() {
		Expect(start(map[string]string{"POD_NAMESPACE": "koinon"}, "ensure-secrets", "--secrets=[]")).To(Succeed())
		Expect(start(nil, "ensure-secrets", "--secrets=[]")).To(MatchError(ContainSubstring(`"namespace" not set`)))
	})

	It("needs the database only for the keys commands that read it", func() {
		kek := map[string]string{"KOINON_KEK_DIR": "/keys"}
		Expect(start(kek, "keys", "add")).To(MatchError(ContainSubstring("--database-url is required")))
		Expect(start(map[string]string{"KOINON_KEK_DIR": "/keys", "DATABASE_URL": db}, "keys", "add")).To(Succeed())
		Expect(start(kek, "keys", "status")).To(MatchError(ContainSubstring("--database-url is required")))
		Expect(start(map[string]string{"KOINON_KEK_DIR": "/keys", "DATABASE_URL": db}, "keys", "status")).To(Succeed())
		Expect(start(kek, "keys", "activate")).To(MatchError(ContainSubstring("accepts 1 arg")))
	})
})
