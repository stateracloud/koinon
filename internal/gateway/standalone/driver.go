// SPDX-License-Identifier: Apache-2.0

package standalone

import (
	"context"
	"errors"
	"sync"

	"github.com/stateracloud/koinon/internal/gateway"
)

// Driver is the standalone ModelGateway: Render, Deliver, Wait.
type Driver struct {
	Options   Options
	Deliverer Deliverer
	Waiter    Waiter

	mu          sync.Mutex
	rejectedRev gateway.Revision
	redelivery  int
}

var _ gateway.ModelGateway = (*Driver)(nil)

// IssueKey implements gateway.ModelGateway. Standalone agentgateway accepts
// keys by hash, so Koinon generates them itself.
func (d *Driver) IssueKey(_ context.Context, agent gateway.AgentRef) (gateway.IssuedKey, error) {
	return gateway.NewKey(agent)
}

// Apply implements gateway.ModelGateway. agentgateway keeps its previous config when it
// rejects a file and does not look at the file again until it changes, so after a rejection the
// next Apply of the same revision delivers the file with a different comment (Options.Redelivery).
//
// One agent's invalid limit fails only that agent: its key is left out of the file and Apply
// returns the revision with a *gateway.RejectedError naming it (a key that is not served is
// refused, so the agent fails closed), as the Kubernetes driver does for an object the controller
// refuses. The file agentgateway would have refused as a whole is never delivered (old issue 245).
func (d *Driver) Apply(ctx context.Context, s gateway.State) (gateway.Revision, error) {
	s, rejected := s.WithoutInvalidLimits()
	rev, err := d.apply(ctx, s)
	if err != nil {
		return "", err
	}
	if len(rejected) > 0 {
		return rev, &gateway.RejectedError{Rejected: rejected}
	}
	return rev, nil
}

func (d *Driver) apply(ctx context.Context, s gateway.State) (gateway.Revision, error) {
	config, rev, err := Render(s, d.Options)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	if rev == d.rejectedRev {
		d.redelivery++
		o := d.Options
		o.Redelivery = d.redelivery
		config, _, err = Render(s, o)
	} else {
		d.rejectedRev, d.redelivery = "", 0
	}
	d.mu.Unlock()
	if err != nil {
		return "", err
	}
	if p, ok := d.Waiter.(interface {
		Prepare(context.Context) context.Context
	}); ok {
		ctx = p.Prepare(ctx)
	}
	if err := d.Deliverer.Deliver(ctx, config, rev); err != nil {
		return "", err
	}
	if d.Waiter != nil {
		if err := d.Waiter.Wait(ctx, rev); err != nil {
			if errors.Is(err, ErrConfigRejected) {
				d.mu.Lock()
				d.rejectedRev = rev
				d.mu.Unlock()
			}
			return "", err
		}
	}
	d.mu.Lock()
	d.rejectedRev, d.redelivery = "", 0
	d.mu.Unlock()
	return rev, nil
}

// Usage implements gateway.ModelGateway. Usage arrives through agentgateway's
// access log, exported over OTLP to the spend ledger in koinon-api (package ledger, L1).
func (d *Driver) Usage() gateway.UsageReader { return gateway.NoUsage{} }
