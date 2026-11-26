// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/execution"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
)

// The actor egress policy (issue #1444, ADR 0120). Substrate denies all egress of an actor that has no rule that
// matches: atenet-egress refuses the connection and its DNS relay answers NXDOMAIN. The driver writes one policy
// per actor that allows cleartext HTTP to the Koinon gateway and nothing else. The policy holds no org host and no
// credential; the gateway does the per-actor checks (ADR 0097). Issue #1386 later adds the replace_headers effect
// to these rules.
//
// The driver writes the policy before every ResumeActor: atenet-egress caches the policy of an actor, and also
// the absence of one, for a short time (--egress-policy-cache-ttl, 10 s by default), so a policy written after
// the guest runs could reach it late. The driver never deletes the policy: Substrate stores it in a row with a
// foreign key to the actor and ON DELETE CASCADE (cmd/ateapi/internal/store/atepg/migrations/000001_initial.sql,
// v1.0.1), so DeleteActor removes it.

// egressPolicyName is the name of the singleton policy under an actor.
const egressPolicyName = "default"

// maxPortsPerRule is Substrate's limit on the port numbers of one rule.
const maxPortsPerRule = 16

// GatewayRoute is one gateway listener that a guest calls: the host name in the guest's gateway URL and the port.
type GatewayRoute struct {
	Host string
	Port uint32
}

func (r GatewayRoute) String() string { return fmt.Sprintf("%s:%d", r.Host, r.Port) }

// ValidateGatewayRoutes checks the routes as Substrate admits an HTTP rule: at least one route, each host a
// lowercase DNS name without a trailing dot, a wildcard or a port, and not an IP address (the DNS relay of
// Substrate answers names only), each port in 1-65535, and at most 16 ports for one host.
func ValidateGatewayRoutes(routes []GatewayRoute) error {
	if len(routes) == 0 {
		return errors.New("no gateway route")
	}
	ports := map[string]map[uint32]bool{}
	for _, r := range routes {
		if err := validHost(r.Host); err != nil {
			return fmt.Errorf("gateway route %s: %w", r, err)
		}
		if r.Port < 1 || r.Port > 65535 {
			return fmt.Errorf("gateway route %s: port must be 1-65535", r)
		}
		if ports[r.Host] == nil {
			ports[r.Host] = map[uint32]bool{}
		}
		ports[r.Host][r.Port] = true
		if len(ports[r.Host]) > maxPortsPerRule {
			return fmt.Errorf("gateway host %q: more than %d ports", r.Host, maxPortsPerRule)
		}
	}
	return nil
}

func validHost(h string) error {
	switch {
	case h == "":
		return errors.New("the host is empty")
	case len(h) > 253:
		return errors.New("the host is longer than 253 characters")
	case h != strings.ToLower(h):
		return errors.New("the host must be lowercase")
	}
	if _, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		return errors.New("the host is an IP address: Substrate egress rules match DNS names only")
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 {
			return errors.New("the host is not a DNS name")
		}
		for i, ch := range label {
			ok := ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-' && i > 0 && i < len(label)-1
			if !ok {
				return fmt.Errorf("the host is not a DNS name (label %q)", label)
			}
		}
	}
	return nil
}

// gatewayRules renders the rules: one HTTP rule for each gateway host, with the ports of that host. Hosts and
// ports are sorted, so the same routes give the same rules.
func gatewayRules(routes []GatewayRoute) []*ateapiv1.EgressRule {
	byHost := map[string][]int32{}
	for _, r := range routes {
		p := int32(r.Port) //nolint:gosec // ValidateGatewayRoutes keeps the port in 1-65535
		if !slices.Contains(byHost[r.Host], p) {
			byHost[r.Host] = append(byHost[r.Host], p)
		}
	}
	hosts := make([]string, 0, len(byHost))
	for h := range byHost {
		hosts = append(hosts, h)
	}
	slices.Sort(hosts)
	rules := make([]*ateapiv1.EgressRule, 0, len(hosts))
	for _, h := range hosts {
		ports := byHost[h]
		slices.SortFunc(ports, cmp.Compare)
		rules = append(rules, &ateapiv1.EgressRule{Http: &ateapiv1.HttpRule{
			Hostnames: []string{h},
			Ports:     &ateapiv1.Ports{Numbers: ports},
		}})
	}
	return rules
}

// requireGatewayRoutes fails closed without gateway routes: the driver would have no policy to write, and an
// actor without one reaches nothing, not even the gateway.
func (d *Driver) requireGatewayRoutes(op string) error {
	if err := ValidateGatewayRoutes(d.cfg.GatewayRoutes); err != nil {
		return fail(op, execution.ErrFailedPrecondition, "egress policy: %v (issue #1444)", err)
	}
	return nil
}

// policyWriteAttempts bounds the retries when another writer changes the policy between the read and the write.
const policyWriteAttempts = 3

// ensureEgressPolicy makes the actor's egress policy hold exactly the gateway rules: it reads the policy, creates
// it when there is none and replaces it when its rules differ. It retries when another writer wins a race
// (AlreadyExists on create, Aborted on update).
func (d *Driver) ensureEgressPolicy(ctx context.Context, op string, k key) error {
	if err := d.requireGatewayRoutes(op); err != nil {
		return err
	}
	want := &ateapiv1.EgressPolicy{
		Metadata: &ateapiv1.ResourceMetadata{Atespace: k.atespace, Name: egressPolicyName},
		Rules:    gatewayRules(d.cfg.GatewayRoutes),
	}
	actor := ref(k.atespace, k.actor)
	var err error
	for range policyWriteAttempts {
		var cur *ateapiv1.EgressPolicy
		cur, err = d.control.GetActorEgressPolicy(ctx, &ateapiv1.GetActorEgressPolicyRequest{Actor: actor, Name: egressPolicyName})
		switch {
		case isCode(err, codeNotFound):
			_, err = d.control.CreateActorEgressPolicy(ctx, &ateapiv1.CreateActorEgressPolicyRequest{Actor: actor, EgressPolicy: want})
			if isCode(err, codeAlreadyExists) {
				continue
			}
		case err != nil:
		case rulesEqual(cur.GetRules(), want.GetRules()):
			return nil
		default:
			upd := proto.Clone(want).(*ateapiv1.EgressPolicy)
			upd.Metadata.Uid, upd.Metadata.Version = cur.GetMetadata().GetUid(), cur.GetMetadata().GetVersion()
			_, err = d.control.UpdateActorEgressPolicy(ctx, &ateapiv1.UpdateActorEgressPolicyRequest{Actor: actor, EgressPolicy: upd})
			if isCode(err, codeAborted) {
				continue
			}
		}
		break
	}
	if err != nil {
		return execution.GRPCError(op+": egress policy", err)
	}
	return nil
}

func rulesEqual(a, b []*ateapiv1.EgressRule) bool {
	return slices.EqualFunc(a, b, func(x, y *ateapiv1.EgressRule) bool { return proto.Equal(x, y) })
}
