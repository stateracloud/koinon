// SPDX-License-Identifier: Apache-2.0

package egress_test

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/forgecredfile"
	"github.com/stateracloud/koinon/internal/gateway/egress"
	"github.com/stateracloud/koinon/internal/gateway/route"
	"github.com/stateracloud/koinon/internal/secretstore"
)

var _ = Describe("SealedCredentials", func() {
	const (
		value = "koinon_test_not-a-real-token"
		org   = "0b9f1a2c-1111-4c2d-8e3f-000000000001"
		agent = "0b9f1a2c-2222-4c2d-8e3f-000000000002"
		other = "0b9f1a2c-3333-4c2d-8e3f-000000000003"
	)
	var (
		gk       *secretstore.RecipientKey
		dir      string
		failures []string
	)
	uuid := func(s string) pgtype.UUID {
		var u pgtype.UUID
		Expect(u.Scan(s)).To(Succeed())
		return u
	}
	// write seals plain for (bindOrg, bindAgent) and stores it under the name of (nameOrg, nameAgent).
	write := func(to *secretstore.RecipientKey, bindOrg, bindAgent, nameOrg, nameAgent, plain string) string {
		env, err := secretstore.SealV2(to.Public(), forgecredfile.Binding(bindOrg, bindAgent), []byte(plain))
		Expect(err).NotTo(HaveOccurred())
		b, err := json.Marshal(env)
		Expect(err).NotTo(HaveOccurred())
		p := filepath.Join(dir, forgecredfile.SealedName(uuid(nameOrg), uuid(nameAgent), b))
		Expect(os.WriteFile(p, b, 0o600)).To(Succeed())
		return p
	}
	open := func(file, credAgent string) (string, error) {
		s := egress.SealedCredentials{Keys: secretstore.NewRecipientKeyring(gk), OnOpenFailure: func(r string) { failures = append(failures, r) }}
		return s.Open(route.Credential{Name: "c", Org: "acme", Agent: credAgent, File: file, Scheme: route.SchemeBasic, User: "x-access-token"})
	}

	BeforeEach(func() {
		var err error
		gk, err = secretstore.GenerateRecipientKey(secretstore.DefaultRecipientSuite())
		Expect(err).NotTo(HaveOccurred())
		dir = GinkgoT().TempDir()
		failures = nil
	})

	It("opens the envelope of the route's agent", func() {
		v, err := open(write(gk, org, agent, org, agent, value+"\n"), agent)
		Expect(err).NotTo(HaveOccurred())
		Expect(v).To(Equal(value))
		Expect(failures).To(BeEmpty())
	})

	It("refuses the file of another agent before it reads it", func() {
		_, err := open(write(gk, org, agent, org, agent, value), other)
		var oe *egress.OpenError
		Expect(err).To(BeAssignableToTypeOf(oe))
		Expect(failures).To(Equal([]string{"format"}))
	})

	It("refuses an envelope renamed to another agent", func() {
		_, err := open(write(gk, org, agent, org, other, value), other)
		Expect(err).To(HaveOccurred())
		Expect(failures).To(Equal([]string{"open"}))
	})

	It("refuses an envelope of another key", func() {
		k2, err := secretstore.GenerateRecipientKey(secretstore.DefaultRecipientSuite())
		Expect(err).NotTo(HaveOccurred())
		_, err = open(write(k2, org, agent, org, agent, value), agent)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).NotTo(ContainSubstring(value))
		Expect(failures).To(Equal([]string{"unknown_key"}))
	})

	It("refuses a plaintext file", func() {
		p := filepath.Join(dir, "forgev2-"+org+"-"+agent+"-0a1b2c3d")
		Expect(os.WriteFile(p, []byte(value), 0o600)).To(Succeed())
		_, err := open(p, agent)
		Expect(err).To(HaveOccurred())
		Expect(failures).To(Equal([]string{"format"}))
	})

	It("refuses a value that cannot go in a header", func() {
		_, err := open(write(gk, org, agent, org, agent, "koinon test\r\nX-Evil: 1"), agent)
		Expect(err).To(HaveOccurred())
		Expect(failures).To(Equal([]string{"format"}))
	})

	It("refuses every credential with no keys", func() {
		p := write(gk, org, agent, org, agent, value)
		_, err := egress.SealedCredentials{}.Open(route.Credential{Agent: agent, File: p})
		Expect(err).To(HaveOccurred())
	})
})
