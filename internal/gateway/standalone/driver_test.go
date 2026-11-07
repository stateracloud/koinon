// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"context"
	"errors"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
	"github.com/stateracloud/koinon/internal/money"
)

type recordingDeliverer struct{ configs []string }

func (d *recordingDeliverer) Deliver(_ context.Context, config []byte, _ gateway.Revision) error {
	d.configs = append(d.configs, string(config))
	return nil
}

type scriptedWaiter struct{ results []error }

func (w *scriptedWaiter) Wait(context.Context, gateway.Revision) error {
	err := w.results[0]
	w.results = w.results[1:]
	return err
}

var _ = Describe("Driver", func() {
	It("redelivers a revision agentgateway rejected with a changed file, and the same revision", func(ctx SpecContext) {
		del := &recordingDeliverer{}
		w := &scriptedWaiter{results: []error{standalone.ErrConfigRejected, standalone.ErrConfigRejected, nil, nil}}
		d := &standalone.Driver{Deliverer: del, Waiter: w}
		s := fixtures()[1].state

		_, err := d.Apply(ctx, s)
		Expect(err).To(MatchError(standalone.ErrConfigRejected))
		_, err = d.Apply(ctx, s)
		Expect(err).To(MatchError(standalone.ErrConfigRejected))
		rev, err := d.Apply(ctx, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(del.configs).To(HaveLen(3))
		Expect(del.configs[0]).NotTo(Equal(del.configs[1]))
		Expect(del.configs[1]).To(ContainSubstring("# Redelivery 1"))
		Expect(del.configs[2]).To(ContainSubstring("# Redelivery 2"))
		// The comment is not part of the revision: the probe key is the same.
		Expect(strings.TrimPrefix(del.configs[1], "# Rendered")).To(ContainSubstring(string(rev)))

		// Once accepted, an unchanged state is delivered unchanged again.
		_, err = d.Apply(ctx, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(del.configs[3]).To(Equal(del.configs[0]))
	})

	It("fails only the key of an agent with an invalid limit, and delivers the rest (old issue 245)", func(ctx SpecContext) {
		del := &recordingDeliverer{}
		d := &standalone.Driver{Deliverer: del, Options: standalone.Options{DatabaseURL: "sqlite:///tmp/x.db"}}
		s := gateway.State{}
		s.AddKey(fixedKey("k-aaaaaaaaaaaa", agentA, "test-a"))
		s.AddKey(fixedKey("k-bbbbbbbbbbbb", agentB, "test-b"))
		s.AddKey(fixedKey("k-gggggggggggg", agentG, "test-g"))
		s.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("0.0172757592"), Window: 24 * time.Hour}) // ten digits
		s.SetLimit(agentB, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("4.98"), Window: 24 * time.Hour})
		s.SetLimit(agentG, &gateway.Limit{Unit: gateway.USD, Amount: -1, Window: 24 * time.Hour})

		rev, err := d.Apply(ctx, s)
		var rej *gateway.RejectedError
		Expect(errors.As(err, &rej)).To(BeTrue(), "%v", err)
		Expect(rev).NotTo(BeEmpty(), "the revision is live for everything else")
		Expect(rej.Rejected).To(HaveLen(2))
		Expect(rej.Rejected[0]).To(Equal(gateway.Rejection{Org: "acme", Kind: "Key", Name: "k-aaaaaaaaaaaa", Reason: "limit: amount 0.017275759200 has more than 9 fraction digits"}))
		Expect(rej.Rejected[1].Org).To(Equal("globex"))
		Expect(rej.Rejected[1].Name).To(Equal("k-gggggggggggg"))
		Expect(rej.Orgs()).To(Equal([]string{"acme", "globex"}))

		Expect(del.configs).To(HaveLen(1))
		cfg := del.configs[0]
		Expect(cfg).To(ContainSubstring(gateway.HashKey("test-b")), "the good agent's key is served")
		Expect(cfg).NotTo(ContainSubstring(gateway.HashKey("test-a")), "a rejected key is not served: it fails closed")
		Expect(cfg).NotTo(ContainSubstring(gateway.HashKey("test-g")))
	})

	It("passes the gateway's reason through when it refuses the whole config", func(ctx SpecContext) {
		reason := &standalone.ConfigRejectedError{Revision: "abc", Reason: "USD budget limits support at most 9 fractional digits"}
		d := &standalone.Driver{Deliverer: &recordingDeliverer{}, Waiter: &scriptedWaiter{results: []error{reason}}}
		_, err := d.Apply(ctx, fixtures()[1].state)
		Expect(err).To(MatchError(standalone.ErrConfigRejected))
		var got *gateway.ConfigRejectedError
		Expect(errors.As(err, &got)).To(BeTrue())
		Expect(got.Reason).To(ContainSubstring("9 fractional digits"))
	})
})
