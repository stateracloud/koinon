// SPDX-License-Identifier: Apache-2.0

package leakguard_test

import (
	"context"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
	"unicode/utf16"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/leakguard"
)

// Bypasses the third review found, and the benign text each fix must leave alone. Every entry
// of "catches" passed the guard before the fix.
var _ = Describe("Evasions of the third review", func() {
	var (
		fp      *leakguard.Fingerprinter
		checker leakguard.TaintChecker
	)
	excerpt := "payroll export job runs nightly on the finance" // 40 canonical characters
	cyrillic := strings.NewReplacer("a", "а", "e", "е", "o", "о", "p", "р", "c", "с", "x", "х").Replace(excerpt)

	BeforeEach(func() {
		fp = newFingerprinter(testKey)
		checker = leakguard.TaintChecker{Store: taintedStore(fp, privateLine)}
	})

	DescribeTable("catches",
		func(args ...string) {
			d, err := checker.Check(context.Background(), testScope, fp, args)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Allow).To(BeFalse(), "%+v", d)
			Expect(d.Reason).To(Equal(leakguard.ReasonTaint))
		},
		// finding 1: punctuation between the letters is neutral to the text test
		Entry("base64 of a '-' after every character", b64(between(excerpt, "-"))),
		Entry("base64 of a '·' after every character", b64(between(excerpt, "·"))),
		Entry("hex of a '-' after every character", hexOf(between(excerpt, "-"))),
		Entry("base32 of a '.' after every character", base32.StdEncoding.EncodeToString([]byte(between(excerpt, ".")))),
		Entry("base64 of a '-' after every character and ' , ' for a space", b64(between(strings.ReplaceAll(excerpt, " ", " , "), "-"))),
		Entry("chunked base64 of a '-' after every character", chunks(b64(between(excerpt, "-")), 15, " ")),
		Entry("base64 of UTF-16LE with a '-' after every character", b64(utf16le(between(excerpt, "-")))),
		// finding 2: only letters and digits count toward the UTF-16 two-block gate
		Entry("base64 of UTF-16LE with an emoji after every character", b64(utf16le(between(excerpt, "😀")))),
		Entry("base64 of UTF-16LE cyrillic with U+3000 between", b64(utf16le(between(cyrillic, "　")))),
		Entry("base64 of UTF-16LE cyrillic with U+2014 between", b64(utf16le(between(cyrillic, "—")))),
		// finding 3: resync reads non-ASCII text too
		Entry("chunked base64 of cyrillic", chunks(b64(cyrillic), 15, " ")),
		Entry("chunked base64 of UTF-16LE cyrillic", chunks(b64(utf16le(cyrillic)), 15, " ")),
		Entry("chunked base64 of fullwidth", chunks(b64(toFullwidth(excerpt)), 15, "\n")),
		Entry("chunked hex of cyrillic", chunks(hexOf(cyrillic), 10, " ")),
		// finding 4: short pieces, each encoded alone and joined
		Entry("base64 in pieces of 13 bytes", pieces(excerpt, 13, " ", base64.StdEncoding.EncodeToString)),
		Entry("base64 in pieces of 14 bytes", pieces(excerpt, 14, " ", base64.StdEncoding.EncodeToString)),
		Entry("base64 in pieces of 15 bytes", pieces(excerpt, 15, "\n", base64.StdEncoding.EncodeToString)),
		Entry("raw base64 in pieces of 13 bytes", pieces(excerpt, 13, " ", base64.RawStdEncoding.EncodeToString)),
		Entry("base32 in pieces of 9 bytes", pieces(excerpt, 9, " ", base32.StdEncoding.EncodeToString)),
		Entry("base32 in pieces of 6 bytes", pieces(excerpt, 6, " ", base32.StdEncoding.EncodeToString)),
		// finding 5: a NUL every 11 bytes
		Entry("base64 with a NUL every 11 bytes", b64(nulEvery(excerpt, 11))),
		Entry("hex with a NUL every 11 bytes", hexOf(nulEvery(excerpt, 11))),
		Entry("base64 with a NUL every 7 bytes", b64(nulEvery(excerpt, 7))),
	)

	It("pairs the arguments of a call with more than 64 of them", func() {
		half := len(excerpt) / 2
		args := spread(100, 3, excerpt[:half], 97, excerpt[half:])
		d, err := checker.Check(context.Background(), testScope, fp, args)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Reason).To(Equal(leakguard.ReasonTaint))
		Expect(d.Match.Args).To(Equal([]int{3, 97}))
	})

	It("lets a benign call of 100 arguments through", func() {
		args := make([]string, 100)
		for i := range args {
			args[i] = fmt.Sprintf("--flag-%d=value-%d", i, i)
		}
		args[10] = strings.Repeat("kubernetes release notes ", 40)
		d, err := checker.Check(context.Background(), testScope, fp, args)
		Expect(err).NotTo(HaveOccurred())
		Expect(d).To(Equal(leakguard.Decision{Allow: true, Reason: leakguard.ReasonPass}))
	})

	It("still fails closed when no pair of the arguments can be checked", func() {
		args := make([]string, 400)
		for i := range args {
			args[i] = strings.Repeat("filler ", 80)
		}
		d, err := checker.Check(context.Background(), testScope, fp, args)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Reason).To(Equal(leakguard.ReasonBudgetArgs))
	})

	It("shortens the join of long arguments instead of running out of time", func() {
		args := make([]string, 64)
		for i := range args {
			args[i] = strings.Repeat("kubernetes release notes ", 30)
		}
		d, err := checker.Check(context.Background(), testScope, fp, args)
		Expect(err).NotTo(HaveOccurred())
		Expect(d).To(Equal(leakguard.Decision{Allow: true, Reason: leakguard.ReasonPass}))
		// the leak is split at the boundary of two long arguments
		half := len(excerpt) / 2
		args[5] = strings.Repeat("padding ", 90) + excerpt[:half]
		args[60] = excerpt[half:] + strings.Repeat(" padding", 90)
		d, err = checker.Check(context.Background(), testScope, fp, args)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Reason).To(Equal(leakguard.ReasonTaint))
	})

	// The clock is a backstop: a benign argument at the size cap, in the slowest shapes, is
	// decided on its content, not on how fast the machine is.
	Describe("an argument at the size cap", func() {
		prose := strings.Repeat("Kubernetes 1.34 deprecates several beta APIs; see the release notes at https://kubernetes.io/docs/. ", 1<<20/100)

		It("has a clock budget of at least a second", func() {
			Expect(leakguard.DefaultLimits().MaxDuration).To(BeNumerically(">=", time.Second))
		})

		DescribeTable("passes",
			func(arg string) {
				Expect(len(arg)).To(BeNumerically("<=", 1<<20))
				d, err := checker.Check(context.Background(), testScope, fp, []string{arg})
				Expect(err).NotTo(HaveOccurred())
				Expect(d).To(Equal(leakguard.Decision{Allow: true, Reason: leakguard.ReasonPass}))
			},
			Entry("plain", prose),
			Entry("base64", b64(prose[:3<<18])),
			Entry("base64 of base64", b64(b64(prose[:9<<16]))),
			Entry("base64 of UTF-16LE", b64(utf16le(prose[:3<<17]))),
		)
	})

	Describe("decoding", func() {
		decodes := func(s string) int {
			vs, err := leakguard.Decode(context.Background(), s, leakguard.Limits{})
			Expect(err).NotTo(HaveOccurred())
			return len(vs)
		}

		// None of these holds encoded text; the fixes must not read any of it as text.
		DescribeTable("leaves benign structured text alone",
			func(s string) { Expect(decodes(s)).To(Equal(1)) },
			Entry("go source", "func (c *blockCodec) block(dst, src []byte) int { var v uint64; for _, s := range src { t := c.table[s]; if t < 0 { return -1 } } }"),
			Entry("json", `{"name":"koinon","version":"0.1.0","dependencies":{"left-pad":"^1.3.0","is-odd":"3.0.1"},"scripts":{"build":"tsc -p .","test":"vitest run"}}`),
			Entry("go.sum", "github.com/onsi/gomega v1.36.2 h1:koinon-test-not-a-real-hash-AAAAAAAAAAAAAAAAAAAAA=\ngithub.com/onsi/gomega v1.36.2/go.mod h1:koinon-test-not-a-real-hash-BBBBBBBBBBBBBBBBBBBBB="),
			Entry("git shas", "commit 35937b0ef31c67bdf4143149e7a996c2d1df21b9 parent 2b3b39440a1c5e0d8e1f2a3b4c5d6e7f80910213"),
			Entry("uuids", "id=123e4567-e89b-12d3-a456-426614174000 other=00000000-0000-0000-0000-000000000000"),
			Entry("png data uri", "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="),
			Entry("pem", "-----BEGIN CERTIFICATE-----\n"+chunks(b64(derLike(600)), 64, "\n")+"\n-----END CERTIFICATE-----"),
			Entry("minified js", `!function(e,t){"use strict";var n=e.document,r=Object.getPrototypeOf,i=r.slice,o=r.concat;function a(e,t){return e-t}window.x=a(1,2)}(window);`),
			Entry("dashed prose", "-------- ---------- -------- ---------- -------- ---------- --------"),
			Entry("punctuation run", "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"),
		)

		// Punctuation, symbols and spaces alone are not text: no decoding of these holds any.
		DescribeTable("does not read symbols as text",
			func(s string) {
				vs, err := leakguard.Decode(context.Background(), s, leakguard.Limits{})
				Expect(err).NotTo(HaveOccurred())
				for _, v := range vs[1:] {
					Expect(v.Text).NotTo(ContainSubstring("-"), "%q", v.Text)
					Expect(v.Text).NotTo(ContainSubstring("😀"), "%q", v.Text)
					Expect(v.Text).NotTo(ContainSubstring("\u3000"), "%q", v.Text)
					Expect(v.Text).NotTo(ContainSubstring(","), "%q", v.Text)
				}
			},
			Entry("base64 of dashes", b64(strings.Repeat("-", 48))),
			Entry("base64 of punctuation", b64(strings.Repeat("-.,;:!?-.,;:!?", 4))),
			Entry("base64 of emoji only", b64(strings.Repeat("😀", 16))),
			Entry("base64 of UTF-16LE emoji only", b64(utf16le(strings.Repeat("😀", 16)))),
			Entry("base64 of ideographic spaces", b64(strings.Repeat("\u3000", 16))),
			Entry("base64 of UTF-16LE ideographic spaces", b64(utf16le(strings.Repeat("\u3000", 16)))),
		)
	})
})

// between returns s with sep after every character.
func between(s, sep string) string {
	var b strings.Builder
	for _, r := range s {
		b.WriteRune(r)
		b.WriteString(sep)
	}
	return b.String()
}

// pieces encodes s in pieces of n bytes, each on its own, and joins them with sep.
func pieces(s string, n int, sep string, enc func([]byte) string) string {
	var out []string
	for len(s) > 0 {
		k := min(n, len(s))
		out = append(out, enc([]byte(s[:k])))
		s = s[k:]
	}
	return strings.Join(out, sep)
}

// nulEvery returns s with a NUL byte after every n bytes.
func nulEvery(s string, n int) string {
	var b strings.Builder
	for i := range len(s) {
		if i > 0 && i%n == 0 {
			b.WriteByte(0)
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// derLike returns n bytes that look like DER: a SEQUENCE header and then bytes without text in them.
func derLike(n int) string {
	b := []byte{0x30, 0x82, byte(n >> 8), byte(n)}
	for i := range n {
		b = append(b, byte(i*167+31))
	}
	return string(b)
}

// Fourth review: benign calls pass inside the clock, and the text tests read the fillers an
// attacker puts between the characters.
var _ = Describe("Fourth review", func() {
	var fp *leakguard.Fingerprinter
	excerpt := "payroll export job runs nightly on the finance" // 40 canonical characters
	chinese := "工资导出任务每天晚上在财务批处理节点上运行并生成报表文件"
	japanese := "給与エクスポートジョブは毎晩財務バッチノードで実行されレポートファイルを生成します"

	BeforeEach(func() { fp = newFingerprinter(testKey) })

	check := func(private string, args ...string) leakguard.Decision {
		checker := leakguard.TaintChecker{Store: taintedStore(fp, private)}
		d, err := checker.Check(context.Background(), testScope, fp, args)
		Expect(err).NotTo(HaveOccurred())
		return d
	}

	DescribeTable("catches",
		func(private string, args ...string) {
			d := check(private, args...)
			Expect(d.Allow).To(BeFalse(), "%+v", d)
			Expect(d.Reason).To(Equal(leakguard.ReasonTaint))
		},
		Entry("UTF-16LE with a pair of emoji between", excerpt, b64(utf16le(between(excerpt, "😀😀")))),
		Entry("UTF-16LE with U+200B between", excerpt, b64(utf16le(between(excerpt, "\u200b")))),
		Entry("UTF-16LE chinese", chinese, b64(utf16le(chinese))),
		Entry("UTF-16BE chinese", chinese, b64(utf16be(chinese))),
		Entry("UTF-16LE chinese with an emoji between", chinese, b64(utf16le(between(chinese, "😀")))),
		Entry("UTF-16LE chinese with U+3000 between", chinese, b64(utf16le(between(chinese, "　")))),
		Entry("UTF-16LE japanese", japanese, b64(utf16le(japanese))),
		Entry("base64 with U+200B between", excerpt, b64(between(excerpt, "\u200b"))),
		Entry("base64 with U+00AD between", excerpt, b64(between(excerpt, "\u00ad"))),
		Entry("base64 with \\x01 between", excerpt, b64(between(excerpt, "\x01"))),
		Entry("base64 with \\x7f between", excerpt, b64(between(excerpt, "\x7f"))),
		Entry("base64 with \\xff between", excerpt, b64(strings.Join(strings.Split(excerpt, ""), "\xff"))),
		Entry("base64 with two combining marks between", excerpt, b64(between(excerpt, "́̂"))),
		Entry("chunked base64 with U+200B between", excerpt, chunks(b64(between(excerpt, "\u200b")), 15, " ")),
	)

	// A benign call is decided on its content, not on a budget: the shapes whose cost the first
	// version of the joined text and the pairs doubled or refused.
	Describe("a benign call passes", func() {
		prose := strings.Repeat("Kubernetes 1.34 deprecates several beta APIs; see the release notes at https://kubernetes.io/docs/. ", 1<<20/100)
		rnd := func() string {
			b := make([]byte, 3<<18)
			r := rand.New(rand.NewPCG(1, 2))
			for i := range b {
				b[i] = byte(r.Uint32())
			}
			return string(b)
		}
		halves := func(s string) []string { return []string{s[:len(s)/2], s[len(s)/2:]} }
		many := func(n, l int) []string {
			a := make([]string, n)
			for i := range a {
				a[i] = strings.Repeat("kubernetes release notes ", l/24+1)[:l]
			}
			return a
		}

		DescribeTable("within the clock",
			func(args []string) {
				start := time.Now()
				d := check(privateLine, args...)
				Expect(d).To(Equal(leakguard.Decision{Allow: true, Reason: leakguard.ReasonPass}))
				Expect(time.Since(start)).To(BeNumerically("<", 3*time.Second))
			},
			Entry("two halves of base64 of base64", halves(b64(b64(prose[:9<<16])))),
			Entry("two halves of base64 of dashed UTF-16", halves(b64(utf16le(between(prose[:3<<16], "-"))))),
			Entry("two halves of base64 of random bytes", halves(b64(rnd()))),
			Entry("two halves of base64 of UTF-16", halves(b64(utf16le(prose[:3<<17])))),
			Entry("65 arguments of 80 bytes", many(65, 80)),
			Entry("100 arguments of 40 bytes", many(100, 40)),
			Entry("128 arguments of 40 bytes", many(128, 40)),
			Entry("64 arguments of 600 bytes", many(64, 600)),
		)
	})

	// Real text in the scripts the guard reads, encoded in the ways it decodes, holds no secret.
	DescribeTable("leaves benign text alone",
		func(s string) {
			Expect(check(privateLine, s)).To(Equal(leakguard.Decision{Allow: true, Reason: leakguard.ReasonPass}))
		},
		Entry("russian", b64(utf16le("Платёжная ведомость формируется каждую ночь на узле финансовой пакетной обработки"))),
		Entry("chinese utf-16", b64(utf16le(chinese))),
		Entry("chinese utf-16 with emoji", b64(utf16le(between(chinese, "😀")))),
		Entry("chinese utf-8", b64(chinese)),
		Entry("japanese", b64(utf16be(japanese))),
		Entry("arabic", b64(utf16le("يتم تشغيل مهمة تصدير الرواتب كل ليلة على عقدة المعالجة المالية"))),
		Entry("hindi", b64(utf16le("वेतन निर्यात कार्य हर रात वित्त बैच नोड पर चलता है और रिपोर्ट फ़ाइल बनाता है"))),
		Entry("greek", b64(utf16le("Η εργασία εξαγωγής μισθοδοσίας εκτελείται κάθε βράδυ στον κόμβο οικονομικής επεξεργασίας"))),
		Entry("emoji only", b64(utf16le(strings.Repeat("😀🎉🚀", 12)))),
		Entry("go.sum", "github.com/onsi/gomega v1.36.2 h1:koinon-test-not-a-real-hash-AAAAAAAAAAAAAAAAAAAAA=\ngithub.com/onsi/gomega v1.36.2/go.mod h1:koinon-test-not-a-real-hash-BBBBBBBBBBBBBBBBBBBBB="),
		Entry("sha and uuid", "35937b0ef31c67bdf4143149e7a996c2d1df21b9 123e4567-e89b-12d3-a456-426614174000"),
		Entry("jwt-like", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJrb2lub24tdGVzdCJ9.koinon_test_not-a-real-signature"),
		Entry("svg data uri", "data:image/svg+xml;base64,"+b64(`<svg xmlns="http://www.w3.org/2000/svg" width="8" height="8"><rect width="8" height="8" fill="#123456"/></svg>`)),
		Entry("css", "body{margin:0;padding:0;font:14px/1.4 system-ui,sans-serif}.a{color:#123456;background:#fafafa}.b{display:flex}"),
		Entry("hexdump", "00000000  7f 45 4c 46 02 01 01 00  00 00 00 00 00 00 00 00  |.ELF............|\n00000010  03 00 3e 00 01 00 00 00  40 10 00 00 00 00 00 00  |..>.....@.......|"),
		Entry("png", "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="),
	)
})

func utf16be(s string) string {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u>>8), byte(u))
	}
	return string(b)
}
