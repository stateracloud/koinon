// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
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
})
