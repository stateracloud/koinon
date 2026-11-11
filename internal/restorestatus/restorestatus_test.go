// SPDX-License-Identifier: Apache-2.0

package restorestatus_test

import (
	"bytes"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/restorestatus"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("Build", func() {
	It("reports a copy: the credentials it refuses, the outward targets and the work a release starts", func(ctx SpecContext) {
		for _, stmt := range []string{`DELETE FROM install_binding`, `DELETE FROM api_tokens`, `DELETE FROM sessions`} {
			_, err := pool.Exec(ctx, stmt)
			Expect(err).NotTo(HaveOccurred())
		}
		org, user, _ := pgtest.NewOrg(ctx, pool)
		_, err := pool.Exec(ctx, `INSERT INTO install_binding (mark, public_url) VALUES ('source-mark-hash', 'https://source.example')`)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, auth_method, created_at, last_seen_at, idle_expires_at, expires_at, install_mark)
			VALUES (sha256('a'), $1, 'password', now(), now(), now() + interval '1 hour', now() + interval '2 hours', 'source-mark-hash'),
			       (sha256('b'), $1, 'password', now(), now(), now() + interval '1 hour', now() + interval '2 hours', 'own-mark-hash')`, user)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `INSERT INTO api_tokens (org_id, name, prefix, token_hash, role, created_by, expires_at, install_mark)
			VALUES ($1, 'ci', 'abcdefgh', sha256('t'), 'admin', $2, now() + interval '1 day', 'source-mark-hash')`, org, user)
		Expect(err).NotTo(HaveOccurred())

		r, err := restorestatus.Build(ctx, pool, "own-mark-hash")
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Binding.State).To(Equal("copy"))
		Expect(r.Binding.RecordedHost.PublicURL).To(Equal("https://source.example"))
		Expect(r.Quarantine.Tokens).To(Equal(1))
		Expect(r.Quarantine.Sessions).To(Equal(1), "a session made on the copy (its own mark) is accepted")

		var out bytes.Buffer
		restorestatus.Print(&out, r)
		Expect(out.String()).To(ContainSubstring("binding:    copy"))
		Expect(strings.Count(out.String(), "refused:")).To(Equal(1))

		By("without a mark the state is unknown and nothing is counted")
		r, err = restorestatus.Build(ctx, pool, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Binding.State).To(Equal("unknown"))
		Expect(r.Quarantine.Tokens + r.Quarantine.Sessions).To(BeZero())
	})
})
