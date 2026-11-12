// SPDX-License-Identifier: Apache-2.0

package keyscli_test

import (
	"bytes"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/secretstore"
)

var _ = Describe("keys compare", func() {
	run := func(ctx SpecContext, store keyscli.KubeStore, in string) (string, error) {
		var out bytes.Buffer
		err := (&keyscli.Tool{Store: store, Out: &out, In: strings.NewReader(in)}).Compare(ctx)
		return out.String(), err
	}

	It("passes for the same key set and names a KEK that is missing or differs, printing no key material", func(ctx SpecContext) {
		mk := func() keyscli.KubeStore {
			s := keyscli.KubeStore{Client: fake.NewClientset(), Namespace: "ns", Name: "koinon-kek"}
			_, err := s.Ensure(ctx, time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC), nil)
			Expect(err).NotTo(HaveOccurred())
			return s
		}
		src, dst := mk(), mk()
		status := func(s keyscli.KubeStore) string {
			ks, err := s.Load(ctx)
			Expect(err).NotTo(HaveOccurred())
			return "KEK\tSTATE\tVALUES\tWRAPPED BY\tFINGERPRINT\n" + ks.Active + "  active  0  local  " + secretstore.Fingerprint(ks.Keys[ks.Active]) + "\n"
		}

		out, err := run(ctx, src, status(src))
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("ok "))

		out, err = run(ctx, dst, status(src))
		Expect(err).To(MatchError(keyscli.ErrCompareFailed))
		Expect(out).To(ContainSubstring("MISSING"))
		Expect(out).To(ContainSubstring("ACTIVE"))

		ks, _ := src.Load(ctx)
		out, err = run(ctx, src, "KEK STATE VALUES WRAPPED FINGERPRINT\n"+ks.Active+" active 0 local 0000000000000000\n")
		Expect(err).To(MatchError(keyscli.ErrCompareFailed))
		Expect(out).To(ContainSubstring("DIFFERS"))

		_, err = run(ctx, src, "no table here\n")
		Expect(err).To(MatchError(ContainSubstring("no KEK table")))
	})
})
