// SPDX-License-Identifier: Apache-2.0

package mcpguard_test

import (
	"encoding/json"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/mcpguard"
	"github.com/stateracloud/koinon/internal/sinkclass"
)

// trusts is a fetch-host trust list for the specs.
type trusts []string

func (t trusts) TrustsHost(h string) bool {
	for _, x := range t {
		if x == h {
			return true
		}
	}
	return false
}

var _ = Describe("splitting a call's arguments (ADR 0091 decision 7)", func() {
	fetch := sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.Fetch, AddressArgs: []string{"url"}}
	nested := sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.Fetch, AddressArgs: []string{"opts.url"}}
	split := func(raw string, d sinkclass.Declaration, hosts sinkclass.HostTrust) (mcpguard.Args, error) {
		return mcpguard.SplitArgs(json.RawMessage(raw), d, hosts)
	}

	It("reads a nested address argument once, as plain keys", func() {
		a, err := split(`{"opts":{"url":"https://docs.example.com/a"},"note":"n"}`, nested, trusts{"docs.example.com"})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.Address).To(Equal(map[string]string{"opts.url": "https://docs.example.com/a"}))
	})

	DescribeTable("refuses an address with more than one reading",
		func(raw string, d sinkclass.Declaration) {
			_, err := split(raw, d, trusts{"trusted.example.com"})
			Expect(err).To(MatchError(mcpguard.ErrAmbiguousAddress))
		},
		Entry("a dotted literal key beside the nested path",
			`{"opts":{"url":"https://evil.example/x"},"opts.url":"https://trusted.example.com/"}`, nested),
		Entry("the nested path beside a dotted literal key (other order)",
			`{"opts.url":"https://trusted.example.com/","opts":{"url":"https://evil.example/x"}}`, nested),
		Entry("a dotted literal key alone", `{"opts.url":"https://trusted.example.com/"}`, nested),
		Entry("a repeated key in the raw JSON", `{"url":"https://trusted.example.com/","url":"https://evil.example/x"}`, fetch),
		Entry("a repeated nested key", `{"opts":{"url":"https://trusted.example.com/","url":"https://evil.example/x"}}`, nested),
		Entry("the same name in another case", `{"url":"https://trusted.example.com/","URL":"https://evil.example/x"}`, fetch),
		Entry("another case alone", `{"Url":"https://trusted.example.com/"}`, fetch),
		Entry("a parent key in another case", `{"OPTS":{"url":"https://trusted.example.com/"}}`, nested),
		Entry("a Unicode case fold (KELVIN SIGN for k)", `{"Key":"https://trusted.example.com/"}`,
			sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.Fetch, AddressArgs: []string{"key"}}),
		Entry("inside a list", `{"opts":[{"url":"https://trusted.example.com/"}]}`, nested),
		Entry("a repeated key of a code-host tool", `{"owner":"northwind","repo":"platform-handbook","repo":"dump"}`,
			sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.CodeHost, AddressArgs: []string{"owner", "repo"}}),
	)

	It("counts a fixed tool's address arguments as payload", func() {
		d := sinkclass.Declaration{Class: sinkclass.ExternalLow, Derivation: sinkclass.Fixed, AddressArgs: []string{"channel"}}
		a, err := split(`{"channel":"general","text":"hello"}`, d, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(a.Address).To(HaveKeyWithValue("channel", "general"))
		Expect(a.Payload).To(Equal([]string{"general", "text", "hello"}))
		Expect(a.PayloadBytes()).To(Equal(len("general") + len("text") + len("hello")))
	})

	Describe("the canonical order of the payload (#256)", func() {
		plain := sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.Fixed}
		// orders is the payload in call order and in canonical order.
		orders := func(raw string) (call, canonical []string) {
			GinkgoHelper()
			a, err := split(raw, plain, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(a.Canonical).To(HaveLen(len(a.Payload)))
			for _, i := range a.Canonical {
				canonical = append(canonical, a.Payload[i])
			}
			Expect(canonical).To(ConsistOf(a.Payload), "a permutation of the payload")
			return a.Payload, canonical
		}

		It("keeps the payload in call order and orders the members by key, whatever order the encoder used", func() {
			call, canon := orders(`{"b":"not-a-real","a":"koinon_test_"}`)
			Expect(call).To(Equal([]string{"b", "not-a-real", "a", "koinon_test_"}))
			Expect(canon).To(Equal([]string{"a", "koinon_test_", "b", "not-a-real"}))
			_, again := orders(`{"a":"koinon_test_","b":"not-a-real"}`)
			Expect(again).To(Equal(canon))
		})

		It("keeps each key next to its value", func() {
			_, canon := orders(`{"zz":"x","half one":"half two"}`)
			Expect(canon).To(Equal([]string{"half one", "half two", "zz", "x"}))
		})

		It("sorts nested objects too, and keeps a list in its order", func() {
			want := []string{"m", "a", "one", "b", "two", "n", "z", "y"}
			_, canon := orders(`{"n":["z","y"],"m":{"b":"two","a":"one"}}`)
			Expect(canon).To(Equal(want))
			_, canon = orders(`{"m":{"a":"one","b":"two"},"n":["z","y"]}`)
			Expect(canon).To(Equal(want))
			_, canon = orders(`{"l":[{"d":"4","c":"3"},{"b":"2","a":"1"}]}`)
			Expect(canon).To(Equal([]string{"l", "c", "3", "d", "4", "a", "1", "b", "2"}))
		})

		It("reads the values in key order with no key between them, nested and in lists too", func() {
			read := func(raw string) []string {
				GinkgoHelper()
				a, err := split(raw, plain, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(a.Values).To(HaveLen(len(a.Payload)))
				var out []string
				for _, i := range a.Values {
					out = append(out, a.Payload[i])
				}
				Expect(out).To(ConsistOf(a.Payload))
				return out
			}
			Expect(read(`{"subject":"half two","body":"half one","to":"x"}`)).To(Equal(
				[]string{"body", "subject", "to", "half one", "half two", "x"}))
			a, err := split(`{"subject":"half one","body":"half two","m":{"z":"1","y":"2"}}`, plain, nil)
			Expect(err).NotTo(HaveOccurred())
			var call []string
			for _, i := range a.CallValues {
				call = append(call, a.Payload[i])
			}
			Expect(call).To(Equal([]string{"subject", "body", "m", "half one", "half two", "z", "y", "1", "2"}), "CallValues keeps call order")
			want := []string{"m", "n", "a", "b", "one", "two", "z", "y"}
			Expect(read(`{"n":["z","y"],"m":{"b":"two","a":"one"}}`)).To(Equal(want))
			Expect(read(`{"m":{"a":"one","b":"two"},"n":["z","y"]}`)).To(Equal(want))
			Expect(read(`{"l":[{"d":"4","c":"3"},{"b":"2","a":"1"}]}`)).To(Equal(
				[]string{"l", "c", "d", "3", "4", "a", "b", "1", "2"}))
		})

		It("keeps a raw repeated key in its order", func() {
			_, canon := orders(`{"k":"first","j":"x","k":"second"}`)
			Expect(canon).To(Equal([]string{"j", "x", "k", "first", "k", "second"}))
		})

		It("puts an address argument's payload at its key's place, without the key", func() {
			fixed := sinkclass.Declaration{Class: sinkclass.ExternalLow, Derivation: sinkclass.Fixed, AddressArgs: []string{"opts.channel"}}
			a, err := split(`{"z":"last","opts":{"note":"n","channel":"general"}}`, fixed, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(a.Address).To(HaveKeyWithValue("opts.channel", "general"))
			Expect(a.Payload).To(Equal([]string{"z", "last", "opts", "note", "n", "general"}))
			var canon []string
			for _, i := range a.Canonical {
				canon = append(canon, a.Payload[i])
			}
			Expect(canon).To(Equal([]string{"opts", "general", "note", "n", "z", "last"}))
		})

		It("orders a non-string address argument's content too", func() {
			fixed := sinkclass.Declaration{Class: sinkclass.ExternalLow, Derivation: sinkclass.Fixed, AddressArgs: []string{"to"}}
			a, err := split(`{"to":{"y":"2","x":"1"},"a":"z"}`, fixed, nil)
			Expect(err).NotTo(HaveOccurred())
			var canon []string
			for _, i := range a.Canonical {
				canon = append(canon, a.Payload[i])
			}
			Expect(a.Payload).To(Equal([]string{"y", "2", "x", "1", "a", "z"}))
			Expect(canon).To(Equal([]string{"a", "z", "x", "1", "y", "2"}))
		})
	})

	It("counts the host labels of a fetch to a host the org does not trust, and not of one it trusts", func() {
		a, err := split(`{"url":"https://c2VjcmV0.evil.example/x?q=1"}`, fetch, trusts{"docs.example.com"})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.Payload).To(Equal([]string{"c2VjcmV0.evil.example/x?q=1"}))
		a, err = split(`{"url":"https://docs.example.com/x?q=1"}`, fetch, trusts{"docs.example.com"})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.Payload).To(Equal([]string{"/x?q=1"}))
		a, err = split(`{"url":"https://docs.example.com/x"}`, fetch, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(a.Payload).To(Equal([]string{"docs.example.com/x"}), "no trust list trusts nothing")
	})

	DescribeTable("counts everything but the host name of a fetch to a host the org trusts",
		func(raw, payload string) {
			a, err := split(`{"url":`+strconv.Quote(raw)+`}`, fetch, trusts{"trusted.example.com"})
			Expect(err).NotTo(HaveOccurred())
			if payload == "" {
				Expect(a.Payload).To(BeEmpty())
				return
			}
			Expect(a.Payload).To(Equal([]string{payload}))
		},
		Entry("userinfo", "https://koinon_test_not-a-real-token@trusted.example.com/x", "koinon_test_not-a-real-token@/x"),
		Entry("user and password", "https://user:koinon_test_pw@trusted.example.com/x", "user:koinon_test_pw@/x"),
		Entry("a port", "https://trusted.example.com:8443/x", ":8443/x"),
		Entry("userinfo and a port, no path", "https://u@trusted.example.com:8443", "u@:8443"),
		Entry("query and fragment", "https://trusted.example.com/x?q=1#frag", "/x?q=1#frag"),
		Entry("the host alone", "https://trusted.example.com", ""),
	)
})
