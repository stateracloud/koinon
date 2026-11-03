// SPDX-License-Identifier: Apache-2.0

package leakguard_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/leakguard"
)

var _ = Describe("Normalise", func() {
	DescribeTable("canonical form",
		func(in, want string) {
			Expect(leakguard.Normalise(in).String()).To(Equal(want))
		},
		Entry("case", "Payroll EXPORT", "payrollexport"),
		Entry("whitespace and separators removed", " pay\troll\n-ex.port_job ", "payrollexportjob"),
		Entry("separators between letters", "p-a-y-r-o-l-l", "payroll"),
		Entry("fullwidth", "ＰＡＹＲＯＬＬ１２", "payroll12"),
		Entry("mathematical bold", "𝐩𝐚𝐲𝐫𝐨𝐥𝐥", "payroll"),
		Entry("ligature", "ﬁnance", "finance"),
		Entry("accents dropped", "café crème", "cafecreme"),
		Entry("combining marks dropped", "pa\u0301yro\u0308ll", "payroll"),
		Entry("zero width characters", "pay\u200broll\u200d\ufeff", "payroll"),
		Entry("bidi controls", "pay\u202eroll\u202c", "payroll"),
		Entry("cyrillic look-alikes", "раyrоll ехроrt", "payrollexport"),
		Entry("greek look-alikes, case-aware", "ΡΑΥΡΟΛΛ ναρ", "paypoλλvap"),
		Entry("sharp s", "Straße STRASSE", "strassestrasse"),
		Entry("dotless i and turkish capital", "ıİ", "ii"),
		Entry("kelvin sign", "K", "k"),
		Entry("other scripts kept", "日本 Ελλάδα", "日本eλλaδa"),
		Entry("invalid UTF-8 is a separator", "pay\xffroll", "payroll"),
		Entry("digits not folded to letters", "pass0rd", "pass0rd"),
		Entry("circled digits", "①②", "12"),
	)

	It("maps each canonical character to its source", func() {
		in := "Ａb-é\u200bc"
		c := leakguard.Normalise(in)
		Expect(c.String()).To(Equal("abec"))
		Expect(c.Off).To(Equal([]int{0, 3, 5, 10}))
	})
})

var _ = Describe("Decode", func() {
	secret := "Northwind Traders renewal is projected"

	texts := func(vs []leakguard.Variant) []string {
		out := make([]string, len(vs))
		for i, v := range vs {
			out[i] = v.Text
		}
		return out
	}

	DescribeTable("finds the decoding",
		func(in, wantChain string) {
			vs, err := leakguard.Decode(context.Background(), in, leakguard.Limits{})
			Expect(err).NotTo(HaveOccurred())
			Expect(vs[0].Text).To(Equal(in))
			var found bool
			for _, v := range vs {
				if v.Chain == wantChain && leakguard.Normalise(v.Text).String() != "" &&
					containsCanon(v.Text, secret) {
					found = true
				}
			}
			Expect(found).To(BeTrue(), "variants: %q", texts(vs))
		},
		Entry("plain", "d="+secret, ""),
		Entry("percent", "d="+percentAll(secret), "percent"),
		Entry("double percent", "d="+percentAll(percentAll(secret)), "percent>percent"),
		Entry("base64 std", "d="+b64(secret), "base64"),
		Entry("base64 url-safe raw", "d="+b64url(secret+"??>>"), "base64"),
		Entry("base64 misaligned inside a path", "https://x.example/abc"+b64(secret), "base64"),
		Entry("hex", "d="+hexOf(secret), "hex"),
		Entry("hex upper, odd offset", "/x"+"A"+strings.ToUpper(hexOf(secret)), "hex"),
		Entry("base64 of percent", b64(percentAll(secret)), "base64>percent"),
		Entry("percent of base64", percentAll(b64(secret)), "percent>base64"),
		Entry("hex of base64", hexOf(b64(secret)), "hex>base64"),
	)

	It("leaves binary decodings alone", func() {
		bin := string([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17})
		vs, err := leakguard.Decode(context.Background(), "x="+b64(bin)+" y="+hexOf(bin), leakguard.Limits{})
		Expect(err).NotTo(HaveOccurred())
		Expect(vs).To(HaveLen(1))
	})

	It("does not decode ordinary words and identifiers", func() {
		vs, err := leakguard.Decode(context.Background(),
			"internationalization KubernetesNetworkPolicy deadbeefcafebabe0123 2026-10-31T12:00:00Z", leakguard.Limits{})
		Expect(err).NotTo(HaveOccurred())
		Expect(vs).To(HaveLen(1))
	})

	It("fails closed beyond the nesting depth", func() {
		s := secret
		for range 5 {
			s = percentAll(s)
		}
		_, err := leakguard.Decode(context.Background(), s, leakguard.Limits{})
		Expect(err).To(MatchError(ContainSubstring("budget_decode_depth")))
	})

	It("fails closed on the variant budget", func() {
		_, err := leakguard.Decode(context.Background(), "a="+b64(secret)+" b="+hexOf(secret)+" c="+percentAll(secret),
			leakguard.Limits{MaxVariants: 2})
		Expect(err).To(MatchError(ContainSubstring("budget_variants")))
	})

	It("fails closed on the work budget", func() {
		_, err := leakguard.Decode(context.Background(), percentAll(secret), leakguard.Limits{MaxWorkBytes: 64})
		Expect(err).To(MatchError(ContainSubstring("budget_work")))
	})
})

func containsCanon(text, want string) bool {
	t := leakguard.Normalise(text).String()
	w := leakguard.Normalise(want).String()
	return len(w) > 0 && len(t) >= len(w) && indexOf(t, w) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
