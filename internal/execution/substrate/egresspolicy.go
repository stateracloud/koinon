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
// per actor that allows cleartext HTTP to the Koinon gateway and nothing else: the model and MCP listeners for
// every actor (Config.GatewayRoutes), and the egress listener (Config.CodeHostEgressRoute) only for an actor whose
// gateway access has CodeHostEgress, which the reconciler gives an agent with a code-host grant
// (SetGatewayAccess). The policy holds no org host and no credential; the gateway does the per-actor checks again
// (ADR 0097). The egress rule carries the replace_headers effect of issue #1386: Substrate puts a JWT for the actor
// in the Koinon-Actor-Token header, so the guest holds no token.
//
// The policy is its own object under the actor, not part of the template: a change of access updates it in place
// and never replaces the sandbox. The driver writes it while the actor is suspended, before every ResumeActor
// (create, restore, start, resume), from the access it holds now. So a resume or a restore never brings back the
// policy an actor had when it was snapshotted or suspended. atenet-egress caches the policy of an actor, and also
// the absence of one, for a short time (--egress-policy-cache-ttl, 10 s by default): a policy written before the
// resume is the one the guest sees, and an update in place reaches a running guest within that time. The driver
// never deletes the policy: Substrate stores it in a row with a foreign key to the actor and ON DELETE CASCADE
// (cmd/ateapi/internal/store/atepg/migrations/000001_initial.sql, v1.0.1), so DeleteActor removes it.

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

// ValidateGatewayRoutes checks the routes as Substrate admits an HTTP rule: at least one route for every actor,
// each host a lowercase DNS name without a trailing dot, a wildcard or a port, and not an IP address (the DNS relay
// of Substrate answers names only), each port in 1-65535, and at most 16 ports for one host. The egress route, when
// set, must not be one of the routes: then every actor would reach it, and the grant would gate nothing.
func ValidateGatewayRoutes(routes []GatewayRoute, egress *GatewayRoute) error {
	if len(routes) == 0 {
		return errors.New("no gateway route")
	}
	all := slices.Clone(routes)
	if egress != nil {
		if slices.Contains(routes, *egress) {
			return fmt.Errorf("gateway egress route %s: it is also a route of every actor, so the code-host grant cannot gate it", *egress)
		}
		all = append(all, *egress)
	}
	ports := map[string]map[uint32]bool{}
	for _, r := range all {
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

// Names that the egress rule carries to the gateway (issue #1386, ADR 0120). They repeat route.TokenHeader and
// actortoken.Audience: the execution layer does not import the gateway packages (the gateway holds the verifier
// and its TLS and HTTP code). A unit test asserts that the values are equal.
const (
	// ActorTokenHeader is the header that Substrate sets on a request to the egress listener.
	ActorTokenHeader = "Koinon-Actor-Token" //nolint:gosec // a header name, not a credential
	// ActorTokenAudience is the audience of the JWT: the Koinon gateway.
	ActorTokenAudience = "koinon-gateway"
	// ActorTokenLifetimeSeconds is the lifetime of each JWT. Substrate allows 300 to 3600 seconds.
	ActorTokenLifetimeSeconds int64 = 900
)

// actorTokenEffects is the effect of the egress rule: Substrate replaces the header with a JWT that it signs for
// the actor that sent the request. The guest holds no token, only a public placeholder value in the header.
func actorTokenEffects() *ateapiv1.HttpRuleEffects {
	return &ateapiv1.HttpRuleEffects{ReplaceHeaders: []*ateapiv1.CredentialHeader{{
		Header: ActorTokenHeader,
		ActorJwt: &ateapiv1.ActorJwtSource{
			Audiences:         []string{ActorTokenAudience},
			ExpirationSeconds: ActorTokenLifetimeSeconds,
		},
	}}}
}

// gatewayRules renders the rules: one HTTP rule for each gateway host, with the ports of that host, and the code-host
// egress route (when set) as its own rule with the actor-token effect. The egress route is a separate rule, also on
// a host that a route of every actor uses: Substrate allows two rules on one host when their ports differ (a tie is a
// pattern with the same port), the exact port makes the egress rule win for its port, and only the winning rule's
// effects apply. So the model and MCP rules carry no effect. Rules are sorted by host, then by ports, so the same
// routes give the same rules.
func gatewayRules(routes []GatewayRoute, egress *GatewayRoute) []*ateapiv1.EgressRule {
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
	rules := make([]*ateapiv1.EgressRule, 0, len(hosts)+1)
	for _, h := range hosts {
		ports := byHost[h]
		slices.SortFunc(ports, cmp.Compare)
		rules = append(rules, &ateapiv1.EgressRule{Http: &ateapiv1.HttpRule{
			Hostnames: []string{h},
			Ports:     &ateapiv1.Ports{Numbers: ports},
		}})
	}
	if egress != nil {
		rules = append(rules, &ateapiv1.EgressRule{Http: &ateapiv1.HttpRule{
			Hostnames: []string{egress.Host},
			Ports:     &ateapiv1.Ports{Numbers: []int32{int32(egress.Port)}}, //nolint:gosec // ValidateGatewayRoutes keeps the port in 1-65535
			Effects:   actorTokenEffects(),
		}})
	}
	slices.SortStableFunc(rules, func(a, b *ateapiv1.EgressRule) int {
		return cmp.Or(
			cmp.Compare(a.GetHttp().GetHostnames()[0], b.GetHttp().GetHostnames()[0]),
			slices.Compare(a.GetHttp().GetPorts().GetNumbers(), b.GetHttp().GetPorts().GetNumbers()),
		)
	})
	return rules
}

// checkActorTokenEffects refuses rules where an actor JWT can reach anything other than the Koinon gateway's egress
// listener (issue #1386; lab test of 2026-11-27). atenet-egress replaces the header on an HttpsRule (MITM) too, and
// then the upstream host, or the guest through an echo, sees the token. So the effect is allowed on exactly one rule:
// the clear-text HttpRule of the egress route, with its one host and its one port. Every other rule, of any kind,
// must carry no replace_headers at all.
func checkActorTokenEffects(rules []*ateapiv1.EgressRule, egress *GatewayRoute) error {
	carriers := 0
	for i, r := range rules {
		switch {
		case len(r.GetHttps().GetEffects().GetReplaceHeaders()) > 0:
			return fmt.Errorf("rule %d: an https rule carries replace_headers; the actor JWT may go only to the gateway egress listener", i)
		case len(r.GetHttp().GetEffects().GetReplaceHeaders()) == 0:
			continue
		}
		h := r.GetHttp()
		if egress == nil || !slices.Equal(h.GetHostnames(), []string{egress.Host}) || h.GetPorts().GetAll() != nil ||
			!slices.Equal(h.GetPorts().GetNumbers(), []int32{int32(egress.Port)}) { //nolint:gosec // ValidateGatewayRoutes keeps the port in 1-65535
			return fmt.Errorf("rule %d (%v): replace_headers on a rule that is not the gateway egress route", i, h.GetHostnames())
		}
		carriers++
	}
	if carriers > 1 {
		return fmt.Errorf("%d rules carry replace_headers; only the one gateway egress rule may", carriers)
	}
	return nil
}

// requireGatewayRoutes fails closed without gateway routes: the driver would have no policy to write, and an
// actor without one reaches nothing, not even the gateway.
func (d *Driver) requireGatewayRoutes(op string) error {
	if err := ValidateGatewayRoutes(d.cfg.GatewayRoutes, d.cfg.CodeHostEgressRoute); err != nil {
		return fail(op, execution.ErrFailedPrecondition, "egress policy: %v (issue #1444)", err)
	}
	return nil
}

// SetGatewayAccess implements [execution.GatewayAccessSetter]. It records the access of the sandbox and makes the
// policy of an actor that exists match it at once in the control plane, in place: a removed grant takes the egress
// listener away from a running actor without a new sandbox. atenet-egress sees the change within its policy cache
// TTL. For an actor that does not exist yet it only records the access, which CreateSandbox and RestoreSandbox
// then write. When the write fails, the actor keeps its old policy until a later call or resume writes the
// recorded access; the gateway checks each call again (ADR 0097). DeleteSandbox drops the record.
func (d *Driver) SetGatewayAccess(ctx context.Context, workspace, name string, access execution.GatewayAccess) error {
	const op = "SetGatewayAccess"
	k := key{workspace, name}
	d.mu.Lock()
	d.access[k] = access
	d.mu.Unlock()
	err := d.ensureEgressPolicy(ctx, op, k)
	if errors.Is(err, execution.ErrNotFound) {
		return nil // no actor yet: CreateSandbox or RestoreSandbox writes the access recorded above
	}
	return err
}

// ForgetGatewayAccess implements [execution.GatewayAccessSetter]. It changes no actor and no policy.
func (d *Driver) ForgetGatewayAccess(workspace, name string) {
	d.forgetAccess(key{workspace, name})
}

// forgetAccess drops the access recorded for the sandbox, so that the record does not outlive the actor (an
// ephemeral agent gets a new sandbox for each task). The reconciler sets the access again before a later create.
func (d *Driver) forgetAccess(k key) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.access, k)
}

// accessOf is the access recorded for the sandbox, or the zero access.
func (d *Driver) accessOf(k key) execution.GatewayAccess {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.access[k]
}

// policyRules are the rules of the actor's policy: the routes of every actor, and the egress listener when its
// access has CodeHostEgress and the install has one.
func (d *Driver) policyRules(k key) []*ateapiv1.EgressRule {
	var egress *GatewayRoute
	if d.cfg.CodeHostEgressRoute != nil && d.accessOf(k).CodeHostEgress {
		egress = d.cfg.CodeHostEgressRoute
	}
	return gatewayRules(d.cfg.GatewayRoutes, egress)
}

// policyWriteAttempts bounds the retries when another writer changes the policy between the read and the write.
const policyWriteAttempts = 3

// ensureEgressPolicy makes the actor's egress policy hold exactly the rules of its access now: it reads the policy,
// creates it when there is none and replaces it when its rules differ. It retries when another writer wins a race
// (AlreadyExists on create, Aborted on update). An error leaves the policy as it was, and the caller does not
// resume the actor.
func (d *Driver) ensureEgressPolicy(ctx context.Context, op string, k key) error {
	if err := d.requireGatewayRoutes(op); err != nil {
		return err
	}
	want := &ateapiv1.EgressPolicy{
		Metadata: &ateapiv1.ResourceMetadata{Atespace: k.atespace, Name: egressPolicyName},
		Rules:    d.policyRules(k),
	}
	var egress *GatewayRoute
	if d.cfg.CodeHostEgressRoute != nil && d.accessOf(k).CodeHostEgress {
		egress = d.cfg.CodeHostEgressRoute
	}
	if err := checkActorTokenEffects(want.GetRules(), egress); err != nil {
		return fail(op, execution.ErrFailedPrecondition, "egress policy: %v (issue #1386)", err)
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
			if isCode(err, codeFailedPrecondition) {
				return fail(op, execution.ErrNotFound, "egress policy: sandbox %s does not exist", k)
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
