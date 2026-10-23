// SPDX-License-Identifier: Apache-2.0

package standalone

import (
	"context"

	"github.com/procoduck/koinon/internal/gateway"
)

// Driver is the standalone ModelGateway: Render, Deliver, Wait.
type Driver struct {
	Options   Options
	Deliverer Deliverer
	Waiter    Waiter
}

var _ gateway.ModelGateway = (*Driver)(nil)

// IssueKey implements gateway.ModelGateway. Standalone agentgateway accepts
// keys by hash, so Koinon generates them itself.
func (d *Driver) IssueKey(_ context.Context, agent gateway.AgentRef) (gateway.IssuedKey, error) {
	return gateway.NewKey(agent)
}

// Apply implements gateway.ModelGateway.
func (d *Driver) Apply(ctx context.Context, s gateway.State) (gateway.Revision, error) {
	config, rev, err := Render(s, d.Options)
	if err != nil {
		return "", err
	}
	if err := d.Deliverer.Deliver(ctx, config, rev); err != nil {
		return "", err
	}
	if d.Waiter != nil {
		if err := d.Waiter.Wait(ctx, rev); err != nil {
			return "", err
		}
	}
	return rev, nil
}

// Usage implements gateway.ModelGateway. Usage arrives through agentgateway's
// access log (L1), not through the driver.
func (d *Driver) Usage() gateway.UsageReader { return gateway.NoUsage{} }
