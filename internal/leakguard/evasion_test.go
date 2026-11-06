// SPDX-License-Identifier: Apache-2.0

package leakguard_test

import (
	"context"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"time"
	"unicode/utf16"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/leakguard"
)

// Bypasses the third review found, and the benign text each fix must leave alone. Every entry
// of "catches" passed the guard before the fix.
// scaledLimits is the product's DefaultLimits with only the wall-clock backstop scaled by
// raceFactor: under -race the decoders run about 10x slower while the work bounds, which count
// bytes, do not change. Every spec here that runs on the default limits takes them from this, so
// none can forget the scaling; the 5 s itself is asserted against DefaultLimits.
func scaledLimits() leakguard.Limits {
	lim := leakguard.DefaultLimits()
	lim.MaxDuration *= raceFactor
	return lim
}

var _ = Describe("Evasions of the third review", func() {
	var (
		fp      *leakguard.Fingerprinter
		checker leakguard.TaintChecker
	)
	excerpt := "payroll export job runs nightly on the finance" // 40 canonical characters
	cyrillic := strings.NewReplacer("a", "а", "e", "е", "o", "о", "p", "р", "c", "с", "x", "х").Replace(excerpt)

	BeforeEach(func() {
		fp = newFingerprinter(testKey)
		checker = leakguard.TaintChecker{Store: taintedStore(fp, privateLine), Limits: scaledLimits()}
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

	It("checks a call of 64 long arguments in time", func() {
		args := make([]string, 64)
		for i := range args {
			args[i] = strings.Repeat("kubernetes release notes ", 30)
		}
		d, err := checker.Check(context.Background(), testScope, fp, args)
		Expect(err).NotTo(HaveOccurred())
		Expect(d).To(Equal(leakguard.Decision{Allow: true, Reason: leakguard.ReasonPass}))
		// the leak is split at the boundary of two adjacent long arguments
		half := len(excerpt) / 2
		args[5] = strings.Repeat("padding ", 90) + excerpt[:half]
		args[6] = excerpt[half:] + strings.Repeat(" padding", 90)
		d, err = checker.Check(context.Background(), testScope, fp, args)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Reason).To(Equal(leakguard.ReasonTaint))
	})

	// The clock is a backstop: a benign argument at the size cap, in the slowest shapes, is
	// decided on its content, not on how fast the machine is.
	Describe("an argument at the size cap", func() {
		prose := strings.Repeat("Kubernetes 1.34 deprecates several beta APIs; see the release notes at https://kubernetes.io/docs/. ", 1<<20/100)

		It("has a clock backstop of 5 seconds", func() {
			Expect(leakguard.DefaultLimits().MaxDuration).To(Equal(5 * time.Second))
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
		checker := leakguard.TaintChecker{Store: taintedStore(fp, private), Limits: scaledLimits()}
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

		// One core: a pod has a small CPU limit, so the workers of the guard are a speed-up
		// only and a benign call has to fit the clock without them.
		var procs int
		BeforeEach(func() { procs = runtime.GOMAXPROCS(1) })
		AfterEach(func() { runtime.GOMAXPROCS(procs) })

		DescribeTable("within the work cap",
			func(args []string, workCap int) {
				// The default work budget decides and the clock is a backstop (5 s): a benign
				// shape is allowed on one core, and uses under half of the budget.
				// Under -race only the clock scales (scaledLimits); the work bounds do not.
				lim := scaledLimits()
				checker := leakguard.TaintChecker{Store: taintedStore(fp, privateLine), Limits: lim}
				d, work, err := checker.CheckWork(context.Background(), testScope, fp, args)
				Expect(err).NotTo(HaveOccurred())
				Expect(d).To(Equal(leakguard.Decision{Allow: true, Reason: leakguard.ReasonPass}))
				Expect(work).To(BeNumerically("<=", int64(workCap)))
				Expect(work).To(BeNumerically("<=", int64(lim.MaxWorkBytes/2)))
			},
			Entry("two halves of base64 of base64", halves(b64(b64(prose[:9<<16]))), 12000000),
			Entry("two halves of base64 of dashed UTF-16", halves(b64(utf16le(between(prose[:3<<16], "-")))), 7400000),
			Entry("two halves of base64 of random bytes", halves(b64(rnd())), 5500000),
			Entry("two halves of base64 of UTF-16", halves(b64(utf16le(prose[:3<<17]))), 7400000),
			Entry("65 arguments of 80 bytes", many(65, 80), 1700000),
			Entry("100 arguments of 40 bytes", many(100, 40), 900000),
			Entry("128 arguments of 40 bytes", many(128, 40), 1500000),
			Entry("64 arguments of 600 bytes", many(64, 600), 700000),
			Entry("128 arguments of 8 KiB, each base64 of UTF-16 of prose", sliced(128, func(i int) string { return b64(utf16le(prose[i*3000 : i*3000+3000])) }), 13800000),
			Entry("128 arguments of 8 KiB, each base64 of random bytes", sliced(128, func(i int) string { return b64(randomBytes(i, 6000)) }), 5600000),
			Entry("100 arguments of 10 KiB, each base64 of random bytes", sliced(100, func(i int) string { return b64(randomBytes(i, 7500)) }), 4900000),
			Entry("65 arguments of 200 bytes, each base64 of UTF-16 of prose", sliced(65, func(i int) string { return b64(utf16le(prose[i*70 : i*70+70])) }), 1700000),
			Entry("128 arguments of 40 bytes, each base64 of random bytes", sliced(128, func(i int) string { return b64(randomBytes(i, 30)) }), 1500000),
			Entry("128 arguments of 3 KiB, each base64 of UTF-16 of Russian", sliced(128, func(i int) string { return b64(utf16le(string([]rune(russianProse)[i*50 : i*50+1500]))) }), 16700000),
			Entry("128 arguments of 8 KiB, each base64 of UTF-16 of Chinese", sliced(128, func(i int) string { return b64(utf16le(string([]rune(chineseProse)[i*20 : i*20+2000]))) }), 14300000),
			Entry("128 arguments of 8 KiB, each base64 of UTF-8 of Russian", sliced(128, func(i int) string { return b64(string([]rune(russianProse)[i*50 : i*50+3000])) }), 15900000),
		)

		// The one spec against the wall clock: the 5 s backstop, on one core, and serial, so that
		// no other spec of a parallel run shares the core. The shape is the slowest one that
		// failed on a loaded lab node (3 CPUs, other packages in parallel).
		It("is allowed within the 5 s backstop and the default work budget on one core", Serial, func() {
			args := sliced(128, func(i int) string { return b64(string([]rune(russianProse)[i*50 : i*50+3000])) })
			// 5 s as shipped; scaled only under -race (scaledLimits).
			d, err := leakguard.TaintChecker{Store: taintedStore(fp, privateLine), Limits: scaledLimits()}.Check(context.Background(), testScope, fp, args)
			Expect(err).NotTo(HaveOccurred())
			Expect(d).To(Equal(leakguard.Decision{Allow: true, Reason: leakguard.ReasonPass}))
		})
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
		Entry("jwt-like", strings.Join([]string{"eyJhbGci", "OiJIUzI1NiJ9", "eyJzdWIi", "OiJrb2lub24tdGVzdCJ9", "koinon_test_not-a-real-signature"}, ".")),
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

func sliced(n int, mk func(i int) string) []string {
	a := make([]string, n)
	for i := range a {
		a[i] = mk(i)
	}
	return a
}

func randomBytes(seed, n int) string {
	r := rand.New(rand.NewPCG(uint64(seed), 7))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return string(b)
}

// Fifth review: every reading that passes the loose gates is checked, not the one that scores
// best.
var _ = Describe("Fifth review", func() {
	var fp *leakguard.Fingerprinter
	BeforeEach(func() { fp = newFingerprinter(testKey) })

	// natural prose that repeats its words: 70% distinct ideographs or fewer
	repeating := "服务器上的数据库密码存放在内部保险库中，每天晚上由财务批处理节点轮换，" +
		"数据库密码轮换完成后，财务批处理节点把报表文件写入内部保险库，" +
		"内部保险库的访问记录每天晚上由服务器上的批处理节点汇总，" +
		"财务批处理节点每天晚上轮换数据库密码并生成报表文件，" +
		"服务器上的报表文件存放在内部保险库的财务目录下面。"
	mixed := "服务器 finance-batch-07 的数据库密码存放在内部保险库 vault 的财务目录下面每天轮换"
	russian := "Платёжная ведомость формируется каждую ночь на узле финансовой пакетной обработки номер семь"

	catches := func(private string, arg string) {
		checker := leakguard.TaintChecker{Store: taintedStore(fp, private), Limits: scaledLimits()}
		d, err := checker.Check(context.Background(), testScope, fp, []string{arg})
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Allow).To(BeFalse(), "%+v", d)
		Expect(d.Reason).To(Equal(leakguard.ReasonTaint))
	}

	It("has prose that repeats its words, under 70% distinct ideographs", func() {
		seen, total := map[rune]bool{}, 0
		for _, r := range repeating {
			if r >= 0x4e00 && r <= 0x9fff {
				seen[r] = true
				total++
			}
		}
		Expect(total).To(BeNumerically(">=", 100))
		Expect(10 * len(seen)).To(BeNumerically("<", 7*total))
	})

	DescribeTable("catches the UTF-16 reading that is not the best scoring",
		func(private, arg string) { catches(private, arg) },
		Entry("chinese prose that repeats words, LE", repeating, b64(utf16le(repeating))),
		Entry("chinese prose that repeats words, BE", repeating, b64(utf16be(repeating))),
		Entry("chinese with latin terms, LE", mixed, b64(utf16le(mixed))),
		Entry("chinese with latin terms, BE", mixed, b64(utf16be(mixed))),
		Entry("russian with spaces, LE", russian, b64(utf16le(russian))),
		Entry("russian with spaces, BE", russian, b64(utf16be(russian))),
		Entry("russian with U+200B between", russian, b64(utf16le(between(russian, "\u200b")))),
		Entry("russian with U+2060 between", russian, b64(utf16le(between(russian, "\u2060")))),
		Entry("russian with U+FEFF between", russian, b64(utf16le(between(russian, "\ufeff")))),
		Entry("chunked base64 of russian with spaces", russian, chunks(b64(utf16le(russian)), 15, " ")),
	)
})

// Sixth review, kept for the call-order join (ADR 0091 decision 6): halves of a leak in adjacent
// arguments of a call of 40.
var _ = Describe("Splits across adjacent arguments of many", func() {
	var fp *leakguard.Fingerprinter
	excerpt := "payroll export job runs nightly on the finance" // 40 canonical characters
	cyrillic := strings.NewReplacer("a", "а", "e", "е", "o", "о", "p", "р", "c", "с", "x", "х").Replace(excerpt)
	BeforeEach(func() { fp = newFingerprinter(testKey) })

	DescribeTable("catches",
		func(private string, first, second string) {
			checker := leakguard.TaintChecker{Store: taintedStore(fp, private), Limits: scaledLimits()}
			args := spread(40, 3, first, 4, second)
			d, err := checker.Check(context.Background(), testScope, fp, args)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Allow).To(BeFalse(), "%+v", d)
			Expect(d.Reason).To(Equal(leakguard.ReasonTaint))
			Expect(d.Match.Args).To(ContainElements(3, 4))
		},
		Entry("plain halves", excerpt, excerpt[:20], excerpt[20:]),
		Entry("halves, each base64", excerpt, b64(excerpt[:21]), b64(excerpt[21:])),
		Entry("base64 cut at a multiple of 4", excerpt, b64(excerpt)[:32], b64(excerpt)[32:]),
		Entry("base64 of UTF-16 cut at a multiple of 4", excerpt, b64(utf16le(excerpt))[:64], b64(utf16le(excerpt))[64:]),
		Entry("one half percent-encoded", excerpt, excerpt[:20], percentAll(excerpt[20:])),
		Entry("hex cut between bytes", excerpt, hexOf(excerpt)[:36], hexOf(excerpt)[36:]),
		Entry("base64 of cyrillic, halves", cyrillic, b64(cyrillic[:len(cyrillic)/2]), b64(cyrillic[len(cyrillic)/2:])),
	)

	It("catches a base64 stream cut inside a block between adjacent arguments of 40", func() {
		checker := leakguard.TaintChecker{Store: taintedStore(fp, excerpt), Limits: scaledLimits()}
		enc := b64(excerpt)
		d, err := checker.Check(context.Background(), testScope, fp, spread(40, 20, enc[:27], 21, enc[27:]))
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Reason).To(Equal(leakguard.ReasonTaint))
	})
})

// Sixth review: a base64 stream of UTF-8 text in a script of two-byte letters reads as text at two
// alignments; each reading that is text is checked, not the one that scores best.
var _ = Describe("Readings of UTF-8 base64", func() {
	var fp *leakguard.Fingerprinter
	BeforeEach(func() { fp = newFingerprinter(testKey) })
	russian := "Платёжная ведомость формируется каждую ночь на узле финансовой пакетной обработки номер семь"
	hindi := "वेतन निर्यात कार्य हर रात वित्त बैच नोड पर चलता है और रिपोर्ट फ़ाइल बनाता है"

	DescribeTable("catches",
		func(private, arg string) {
			checker := leakguard.TaintChecker{Store: taintedStore(fp, private), Limits: scaledLimits()}
			d, err := checker.Check(context.Background(), testScope, fp, []string{arg})
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Reason).To(Equal(leakguard.ReasonTaint))
		},
		Entry("russian, std", russian, b64(russian)),
		Entry("russian, url", russian, b64url(russian)),
		Entry("hindi, std", hindi, b64(hindi)),
		Entry("hindi, url", hindi, b64url(hindi)),
		Entry("russian, shifted by one", russian, b64("x"+russian)),
		Entry("russian, shifted by two", russian, b64("xy"+russian)),
		Entry("hindi, shifted by one", hindi, b64("x"+hindi)),
	)
})

// Sixth review, second finding, kept for the call-order join: a stream cut anywhere between two
// adjacent arguments of a call of 40, padded with arguments that hold base64 letters.
var _ = Describe("Streams cut inside an encoding unit in a call of many arguments", func() {
	var fp *leakguard.Fingerprinter
	excerpt := "payroll export job runs nightly on the finance" // 40 canonical characters
	BeforeEach(func() { fp = newFingerprinter(testKey) })

	padded := func(n, i int, a string, j int, b string) []string {
		out := make([]string, n)
		for k := range out {
			out[k] = []string{"filler " + fmt.Sprint(k), "true", "42", "--verbose", "report"}[k%5]
		}
		out[i], out[j] = a, b
		return out
	}

	DescribeTable("catches",
		func(first, second string) {
			checker := leakguard.TaintChecker{Store: taintedStore(fp, excerpt), Limits: scaledLimits()}
			d, err := checker.Check(context.Background(), testScope, fp, padded(40, 1, first, 2, second))
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Reason).To(Equal(leakguard.ReasonTaint))
			Expect(d.Match.Args).To(ContainElements(1, 2))
		},
		Entry("base64 cut at 27", b64raw(excerpt)[:27], b64raw(excerpt)[27:]),
		Entry("base64 cut at 30", b64raw(excerpt)[:30], b64raw(excerpt)[30:]),
		Entry("base64 cut at 33", b64raw(excerpt)[:33], b64raw(excerpt)[33:]),
		Entry("url base64 cut at 31", b64url(excerpt)[:31], b64url(excerpt)[31:]),
		Entry("hex cut at 41", hexOf(excerpt)[:41], hexOf(excerpt)[41:]),
		Entry("hex cut at 40", hexOf(excerpt)[:40], hexOf(excerpt)[40:]),
		Entry("base64 of UTF-16 cut at 66", b64raw(utf16le(excerpt))[:66], b64raw(utf16le(excerpt))[66:]),
	)
})

var (
	russianProse = strings.Repeat("Платёжная ведомость формируется каждую ночь на узле финансовой пакетной обработки. ", 400)
	chineseProse = strings.Repeat("服务器上的数据库密码存放在内部保险库中，每天晚上由财务批处理节点轮换。", 400)
)

// Invalid UTF-8 bytes inside a stream are noise the sender controls, as fillers are: the text
// around them is still the secret.
var _ = Describe("Invalid bytes interleaved in an encoded stream", func() {
	var fp *leakguard.Fingerprinter
	BeforeEach(func() { fp = newFingerprinter(testKey) })
	prose := "Payroll export job runs nightly on the finance batch node number seven and writes the report file"
	russian := "Платёжная ведомость формируется каждую ночь на узле финансовой пакетной обработки номер семь"

	// noisy puts num invalid bytes (kinds of them, cycling) in every den characters, spread evenly.
	noisy := func(s string, num, den int, kinds string) string {
		var b strings.Builder
		n := 0
		for i, r := range []rune(s) {
			b.WriteRune(r)
			for ; n < (i+1)*num/den; n++ {
				b.WriteByte(kinds[n%len(kinds)])
			}
		}
		return b.String()
	}
	type share struct{ num, den int }
	// the shares of the bytes that are invalid: prose is one byte a character, Russian two
	five, twelve, thirty := share{1, 19}, share{3, 22}, share{3, 7}
	rFive, rTwelve, rThirty := share{1, 10}, share{1, 4}, share{4, 5}
	one, many := "\xff", "\xff\xfe\x80\xc0\xf5\xa0"

	DescribeTable("catches",
		func(private, arg string) {
			checker := leakguard.TaintChecker{Store: taintedStore(fp, private), Limits: scaledLimits()}
			d, err := checker.Check(context.Background(), testScope, fp, []string{arg})
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Reason).To(Equal(leakguard.ReasonTaint))
		},
		Entry("prose, 5%, one kind", prose, b64(noisy(prose, five.num, five.den, one))),
		Entry("prose, 5%, many kinds", prose, b64(noisy(prose, five.num, five.den, many))),
		Entry("prose, about 12%, one kind", prose, b64(noisy(prose, twelve.num, twelve.den, one))),
		Entry("prose, about 12%, many kinds", prose, b64(noisy(prose, twelve.num, twelve.den, many))),
		Entry("prose, about 30%, one kind", prose, b64(noisy(prose, thirty.num, thirty.den, one))),
		Entry("prose, about 30%, many kinds", prose, b64(noisy(prose, thirty.num, thirty.den, many))),
		Entry("prose, 30%, many kinds, url alphabet", prose, b64url(noisy(prose, thirty.num, thirty.den, many))),
		Entry("prose, 12%, hex", prose, hex.EncodeToString([]byte(noisy(prose, twelve.num, twelve.den, many)))),
		Entry("prose, 12%, base32", prose, base32.StdEncoding.EncodeToString([]byte(noisy(prose, twelve.num, twelve.den, many)))),
		Entry("russian, 5%, many kinds", russian, b64(noisy(russian, rFive.num, rFive.den, many))),
		Entry("russian, 12%, many kinds", russian, b64(noisy(russian, rTwelve.num, rTwelve.den, many))),
		Entry("russian, 30%, many kinds", russian, b64(noisy(russian, rThirty.num, rThirty.den, many))),
	)

	It("passes base64 of random bytes", func() {
		checker := leakguard.TaintChecker{Store: taintedStore(fp, prose), Limits: scaledLimits()}
		for seed := range 40 {
			d, err := checker.Check(context.Background(), testScope, fp, []string{b64(randomBytes(seed, 600)), b64(randomBytes(seed+99, 40))})
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Reason).NotTo(Equal(leakguard.ReasonTaint))
		}
	})
})

// The work budget decides, on every machine: with no clock to speak of (an hour), a call that
// blows up decoding still ends on a budget of work, pairs or depth, never of time.
var _ = Describe("Blow-ups end on the work budget", func() {
	var fp *leakguard.Fingerprinter
	BeforeEach(func() { fp = newFingerprinter(testKey) })
	prose := strings.Repeat("Kubernetes 1.34 deprecates several beta APIs; see the release notes at https://kubernetes.io/docs/. ", 1<<20/100)

	DescribeTable("fails closed on",
		func(args []string, lim leakguard.Limits, want leakguard.Reason) {
			lim.MaxDuration = time.Hour
			checker := leakguard.TaintChecker{Store: taintedStore(fp, "payroll export job runs nightly"), Limits: lim}
			d, err := checker.Check(context.Background(), testScope, fp, args)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Allow).To(BeFalse())
			Expect(d.Reason).To(Equal(want))
		},
		Entry("nesting beyond the depth", []string{percentAll(percentAll(percentAll(percentAll(percentAll("payroll export job runs nightly")))))}, leakguard.Limits{}, leakguard.ReasonDecodeDepth),
		Entry("a megabyte of base64 of base64 on a small work budget", []string{b64(b64(prose[:9<<16]))}, leakguard.Limits{MaxWorkBytes: 1 << 20}, leakguard.ReasonBudgetWork),
		Entry("many long arguments on a small work budget", sliced(100, func(i int) string { return b64(randomBytes(i, 6000)) }), leakguard.Limits{MaxWorkBytes: 1 << 20}, leakguard.ReasonBudgetWork),
		Entry("prose projected into every alphabet on a small work budget", []string{prose[:1<<19]}, leakguard.Limits{MaxWorkBytes: 1 << 20}, leakguard.ReasonBudgetWork),
	)
})
