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
			ws, err := wrapping.New(ctx, parse(), role)
			Expect(err).NotTo(HaveOccurred())
			Expect(ws.Primary().Backend()).To(Equal(secretstore.BackendLocal))
			_, err = ws.Wrap(ctx, "koinon-test", kek)
			Expect(err).NotTo(HaveOccurred())
		}
	})

	It("builds the OpenBao primary from the flags without calling it, and a service may not wrap", func(ctx SpecContext) {
		cfg := parse("--kek-backend=openbao", "--openbao-address=https://bao.example:8200",
			"--openbao-key=koinon-kek", "--openbao-role=koinon-api", "--openbao-transit-mount=koinon/transit")
		ws, err := wrapping.New(ctx, cfg, wrapping.Service)
		Expect(err).NotTo(HaveOccurred())
		Expect(ws.Primary().Backend()).To(Equal(wrapping.BackendOpenBao))
		Expect(ws.Primary().KeyID()).To(Equal("koinon/transit/koinon-kek"))
		_, err = ws.Wrap(context.Background(), "koinon-test", kek)
		Expect(err).To(MatchError(secretstore.ErrKeyAdminWraps))

		// A KEK stored before the switch still opens.
		local, err := wrapping.New(ctx, parse(), wrapping.Service)
		Expect(err).NotTo(HaveOccurred())
		stored, err := local.Wrap(ctx, "koinon-test", kek)
		Expect(err).NotTo(HaveOccurred())
		got, err := ws.Unwrap(ctx, "koinon-test", stored)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(kek))
	})

	It("refuses an unknown backend and an incomplete OpenBao configuration", func(ctx SpecContext) {
		_, err := wrapping.New(ctx, parse("--kek-backend=vaultish"), wrapping.Service)
		Expect(err).To(MatchError(ContainSubstring("unknown KEK backend")))
		_, err = wrapping.New(ctx, parse("--kek-backend=openbao", "--openbao-address=https://bao.example"), wrapping.KeyAdmin)
		Expect(err).To(HaveOccurred())
		_, err = wrapping.New(ctx, parse("--kek-backend=openbao", "--openbao-address=https://bao.example",
			"--openbao-key=koinon-kek", "--openbao-role=koinon-api", "--openbao-namespace=team/../root"), wrapping.Service)
		Expect(err).To(MatchError(ContainSubstring("invalid namespace")))
	})

	It("warns when a local process is given OpenBao settings, and only then", func(ctx SpecContext) {
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		DeferCleanup(func() { slog.SetDefault(prev) })

		_, err := wrapping.New(ctx, parse(), wrapping.Service)
		Expect(err).NotTo(HaveOccurred())
		_, err = wrapping.New(ctx, wrapping.Config{}, wrapping.Service)
		Expect(err).NotTo(HaveOccurred())
		Expect(logs.String()).To(BeEmpty(), "the flags' defaults are no settings")

		ws, err := wrapping.New(ctx, parse("--openbao-address=https://bao.example:8200", "--openbao-key=koinon-kek"), wrapping.Service)
		Expect(err).NotTo(HaveOccurred())
		Expect(ws.Primary().Backend()).To(Equal(secretstore.BackendLocal))
		Expect(logs.String()).To(ContainSubstring("OpenBao settings are ignored"))
		Expect(logs.String()).To(ContainSubstring("openbao-address"))
		Expect(logs.String()).To(ContainSubstring("openbao-key"))
	})
})
