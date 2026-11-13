// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"bytes"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/installbinding"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("koinon-api restore release-check (the pre-upgrade Job, ADR 0110 decision 1)", func() {
	run := func(dsn string, args ...string) (string, error) {
		GinkgoHelper()
		cmd := newRestoreCommand()
		out := &bytes.Buffer{}
		cmd.SetOut(out)
		cmd.SetErr(out)
		cmd.SetArgs(append([]string{"release-check", "--database-url", dsn}, args...))
		err := cmd.ExecuteContext(GinkgoT().Context())
		return out.String(), err
	}

	It("exits 0 on a first install, before the schema exists", func(ctx SpecContext) {
		out, err := run(pgtest.Run(ctx))
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("a first install"))
	})

	It("exits 0 on a live install with nothing pending and nothing waiting", func(ctx SpecContext) {
		pool := pgtest.Start(ctx)
		mark := "koinon_test_not-a-real-mark"
		_, err := installbinding.New(pool, execution.OwnerID(mark), false, nil).Claim(ctx, installbinding.Targets{})
		Expect(err).NotTo(HaveOccurred())
		out, err := run(connString(pool), "--install-mark", mark)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("nothing to check"))
	})
})

func connString(p *pgxpool.Pool) string { return p.Config().ConnString() }
