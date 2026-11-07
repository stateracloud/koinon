// SPDX-License-Identifier: Apache-2.0

package wrapping_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/pflag"

	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/wrapping"
)

func TestWrapping(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "wrapping Suite")
}

// unset is the bound-only marker of an install that has not set it (old issue 615).
var unset = secretstore.StaticBoundOnly(secretstore.BoundOnly{})

func parse(args ...string) wrapping.Config {
	GinkgoHelper()
	var c wrapping.Config
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	c.Flags(fs)
	Expect(fs.Parse(args)).To(Succeed())
	return c
}

var _ = Describe("New", func() {
	kek := bytes.Repeat([]byte("k"), secretstore.KeySize) // obviously fake

	It("is the local KEK by default, which every role wraps with", func(ctx SpecContext) {
		for _, role := range []wrapping.Role{wrapping.Service, wrapping.KeyAdmin} {
			ws, err := wrapping.New(ctx, parse(), role, unset)
			Expect(err).NotTo(HaveOccurred())
			Expect(ws.Primary().Backend()).To(Equal(secretstore.BackendLocal))
			_, err = ws.Wrap(ctx, "koinon-test", kek)
			Expect(err).NotTo(HaveOccurred())
		}
	})

	It("builds the OpenBao primary from the flags without calling it, and a service may not wrap", func(ctx SpecContext) {
		cfg := parse("--kek-backend=openbao", "--openbao-address=https://bao.example:8200",
			"--openbao-key=koinon-kek", "--openbao-role=koinon-api", "--openbao-transit-mount=koinon/transit")
		ws, err := wrapping.New(ctx, cfg, wrapping.Service, unset)
		Expect(err).NotTo(HaveOccurred())
		Expect(ws.Primary().Backend()).To(Equal(wrapping.BackendOpenBao))
		Expect(ws.Primary().KeyID()).To(Equal("koinon/transit/koinon-kek"))
		_, err = ws.Wrap(context.Background(), "koinon-test", kek)
		Expect(err).To(MatchError(secretstore.ErrKeyAdminWraps))

		// A KEK stored before the switch still opens.
		local, err := wrapping.New(ctx, parse(), wrapping.Service, unset)
		Expect(err).NotTo(HaveOccurred())
		stored, err := local.Wrap(ctx, "koinon-test", kek)
		Expect(err).NotTo(HaveOccurred())
		got, err := ws.Unwrap(ctx, "koinon-test", stored)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(kek))
	})

	It("refuses to build wrappers without a source for the bound-only marker (old issue 615), in every role and backend", func(ctx SpecContext) {
		for _, role := range []wrapping.Role{wrapping.Service, wrapping.KeyAdmin} {
			for _, cfg := range []wrapping.Config{parse(), parse("--kek-backend=openbao", "--openbao-address=https://bao.example:8200",
				"--openbao-key=koinon-kek", "--openbao-role=koinon-api")} {
				_, err := wrapping.New(ctx, cfg, role, nil)
				Expect(err).To(MatchError(secretstore.ErrNoBoundOnlySource), cfg.Backend)
			}
		}
	})

	It("refuses an unknown backend and an incomplete OpenBao configuration", func(ctx SpecContext) {
		_, err := wrapping.New(ctx, parse("--kek-backend=vaultish"), wrapping.Service, unset)
		Expect(err).To(MatchError(ContainSubstring("unknown KEK backend")))
		_, err = wrapping.New(ctx, parse("--kek-backend=openbao", "--openbao-address=https://bao.example"), wrapping.KeyAdmin, unset)
		Expect(err).To(HaveOccurred())
		_, err = wrapping.New(ctx, parse("--kek-backend=openbao", "--openbao-address=https://bao.example",
			"--openbao-key=koinon-kek", "--openbao-role=koinon-api", "--openbao-namespace=team/../root"), wrapping.Service, unset)
		Expect(err).To(MatchError(ContainSubstring("invalid namespace")))
	})

	It("builds the AWS KMS primary from the flags and the pod's IRSA identity, and a service may not wrap", func(ctx SpecContext) {
		GinkgoT().Setenv("AWS_ROLE_ARN", "arn:aws:iam::111122223333:role/koinon-api")
		GinkgoT().Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "/var/run/secrets/eks.amazonaws.com/serviceaccount/token")
		key := "arn:aws:kms:eu-central-1:111122223333:key/0b1d2e3f-4a5b-4c6d-8e9f-a0b1c2d3e4f5"
		ws, err := wrapping.New(ctx, parse("--kek-backend=awskms", "--awskms-region=eu-central-1", "--awskms-key-id="+key), wrapping.Service, unset)
		Expect(err).NotTo(HaveOccurred())
		Expect(ws.Primary().Backend()).To(Equal(wrapping.BackendAWSKMS))
		Expect(ws.Primary().KeyID()).To(Equal(key))
		_, err = ws.Wrap(ctx, "koinon-test", kek)
		Expect(err).To(MatchError(secretstore.ErrKeyAdminWraps))
	})

	It("refuses AWS KMS without a pod identity, even with static keys in the environment", func(ctx SpecContext) {
		for _, k := range []string{"AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"} {
			GinkgoT().Setenv(k, "")
		}
		GinkgoT().Setenv("AWS_ACCESS_KEY_ID", "koinon_test_not-a-real-key-id")
		GinkgoT().Setenv("AWS_SECRET_ACCESS_KEY", "koinon_test_not-a-real-secret")
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		DeferCleanup(func() { slog.SetDefault(prev) })
		_, err := wrapping.New(ctx, parse("--kek-backend=awskms", "--awskms-region=eu-central-1", "--awskms-key-id=alias/koinon-kek"), wrapping.KeyAdmin, unset)
		Expect(err).To(MatchError(ContainSubstring("no pod identity")))
		Expect(logs.String()).To(ContainSubstring("static AWS keys are ignored"))
		Expect(logs.String()).NotTo(ContainSubstring("not-a-real"))
	})

	It("warns when a local process is given AWS KMS settings", func(ctx SpecContext) {
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		DeferCleanup(func() { slog.SetDefault(prev) })
		ws, err := wrapping.New(ctx, parse("--awskms-region=eu-central-1", "--awskms-key-id=alias/koinon-kek"), wrapping.Service, unset)
		Expect(err).NotTo(HaveOccurred())
		Expect(ws.Primary().Backend()).To(Equal(secretstore.BackendLocal))
		Expect(logs.String()).To(ContainSubstring("AWS KMS settings are ignored"))
		Expect(logs.String()).To(ContainSubstring("awskms-key-id"))
	})

	It("warns when a local process is given OpenBao settings, and only then", func(ctx SpecContext) {
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		DeferCleanup(func() { slog.SetDefault(prev) })

		_, err := wrapping.New(ctx, parse(), wrapping.Service, unset)
		Expect(err).NotTo(HaveOccurred())
		_, err = wrapping.New(ctx, wrapping.Config{}, wrapping.Service, unset)
		Expect(err).NotTo(HaveOccurred())
		Expect(logs.String()).To(BeEmpty(), "the flags' defaults are no settings")

		ws, err := wrapping.New(ctx, parse("--openbao-address=https://bao.example:8200", "--openbao-key=koinon-kek"), wrapping.Service, unset)
		Expect(err).NotTo(HaveOccurred())
		Expect(ws.Primary().Backend()).To(Equal(secretstore.BackendLocal))
		Expect(logs.String()).To(ContainSubstring("OpenBao settings are ignored"))
		Expect(logs.String()).To(ContainSubstring("openbao-address"))
		Expect(logs.String()).To(ContainSubstring("openbao-key"))
	})
})
