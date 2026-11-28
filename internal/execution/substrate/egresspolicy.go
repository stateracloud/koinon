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
// (ADR 0097). Every gateway rule carries the replace_headers effect of issues #1386 and #1489: Substrate puts a JWT
// for the actor, minted for the audience of that one listener, in the listener's header, so the guest holds no
// token. The model and MCP listeners read it from Authorization (prefix "Bearer "), where the guest's client sends a
// public placeholder; the egress listener reads it from Koinon-Actor-Token.
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

// GatewayRoute is one gateway listener that a guest calls: the host name in the guest's gateway URL and the port,
// and where the listener reads the actor JWT. Make one with [ModelRoute], [MCPRoute] or [EgressRoute].
type GatewayRoute struct {
	Host string
	Port uint32
	// Header is the request header that Substrate replaces with the actor JWT, and Prefix the text before the
	// JWT in it ("Bearer " for Authorization).
	Header, Prefix string
	// Audience is the one audience of the JWT: the listener's own (issue #1489), so a token for one listener is
	// refused on every other listener.
	Audience string
}

// ModelRoute is koinon-gateway's model listener at host:port.
func ModelRoute(host string, port uint32) GatewayRoute {
	return GatewayRoute{Host: host, Port: port, Header: AuthorizationHeader, Prefix: BearerPrefix, Audience: AudienceModel}
}

// MCPRoute is koinon-gateway's MCP listener at host:port.
func MCPRoute(host string, port uint32) GatewayRoute {
	return GatewayRoute{Host: host, Port: port, Header: AuthorizationHeader, Prefix: BearerPrefix, Audience: AudienceMCP}
}

// EgressRoute is koinon-gateway's egress listener at host:port. It keeps the Koinon-Actor-Token header: the
// guest's git and HTTP clients set Authorization for the upstream host themselves.
func EgressRoute(host string, port uint32) GatewayRoute {
	return GatewayRoute{Host: host, Port: port, Header: ActorTokenHeader, Audience: AudienceEgress}
}

// sameListener reports whether r and o are the same host and port.
func (r GatewayRoute) sameListener(o GatewayRoute) bool { return r.Host == o.Host && r.Port == o.Port }

func (r GatewayRoute) String() string { return fmt.Sprintf("%s:%d", r.Host, r.Port) }

// ValidateGatewayRoutes checks the routes as Substrate admits an HTTP rule: at least one route for every actor,
// each host a lowercase DNS name without a trailing dot, a wildcard or a port, and not an IP address (the DNS relay
// of Substrate answers names only), and each port in 1-65535. Each route is its own
// listener with its own audience (issue #1489): two routes on one host and port are refused, and each route must
// have a header and one of the listener audiences. The egress route, when set, must not be one of the routes: then
// every actor would reach it, and the grant would gate nothing.
func ValidateGatewayRoutes(routes []GatewayRoute, egress *GatewayRoute) error {
	if len(routes) == 0 {
		return errors.New("no gateway route")
	}
	all := slices.Clone(routes)
	if egress != nil {
		if slices.ContainsFunc(routes, egress.sameListener) {
			return fmt.Errorf("gateway egress route %s: it is also a route of every actor, so the code-host grant cannot gate it", *egress)
		}
		if egress.Audience != AudienceEgress {
			return fmt.Errorf("gateway egress route %s: the audience must be %q, not %q", *egress, AudienceEgress, egress.Audience)
		}
		all = append(all, *egress)
	}
	for i, r := range all {
		if slices.ContainsFunc(all[:i], r.sameListener) {
			return fmt.Errorf("gateway route %s: two listeners on one host and port; each listener has its own audience", r)
		}
		if r.Header == "" {
			return fmt.Errorf("gateway route %s: no actor token header", r)
		}
		if !slices.Contains(listenerAudiences, r.Audience) {
			return fmt.Errorf("gateway route %s: audience %q is not a gateway listener audience", r, r.Audience)
		}
		if err := validHost(r.Host); err != nil {
			return fmt.Errorf("gateway route %s: %w", r, err)
		}
		if r.Port < 1 || r.Port > 65535 {
			return fmt.Errorf("gateway route %s: port must be 1-65535", r)
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

// Names that the gateway rules carry to the gateway (issues #1386 and #1489, ADR 0120). They repeat
// route.TokenHeader and the actortoken listener audiences: the execution layer does not import the gateway packages
// (the gateway holds the verifier and its TLS and HTTP code). A unit test asserts that the values are equal.
const (
	// ActorTokenHeader is the header that Substrate sets on a request to the egress listener.
	ActorTokenHeader = "Koinon-Actor-Token" //nolint:gosec // a header name, not a credential
	// AuthorizationHeader and BearerPrefix carry the JWT to the model and MCP listeners.
	AuthorizationHeader = "Authorization"
	BearerPrefix        = "Bearer "
	// AudienceModel, AudienceMCP and AudienceEgress are the audiences of the three listeners.
	AudienceModel  = "koinon-gateway-model"
	AudienceMCP    = "koinon-gateway-mcp"
	AudienceEgress = "koinon-gateway-egress"
	// ActorTokenLifetimeSeconds is the lifetime of each JWT. Substrate allows 300 to 3600 seconds.
	ActorTokenLifetimeSeconds int64 = 900
)

var listenerAudiences = []string{AudienceModel, AudienceMCP, AudienceEgress}

// actorTokenEffects is the effect of the rule of route r: Substrate replaces r's header with a JWT that it signs for
// the actor that sent the request, for r's audience only. The guest holds no token, only a public placeholder.
func actorTokenEffects(r GatewayRoute) *ateapiv1.HttpRuleEffects {
	return &ateapiv1.HttpRuleEffects{ReplaceHeaders: []*ateapiv1.CredentialHeader{{
		Header: r.Header,
		Prefix: r.Prefix,
		ActorJwt: &ateapiv1.ActorJwtSource{
			Audiences:         []string{r.Audience},
			ExpirationSeconds: ActorTokenLifetimeSeconds,
		},
	}}}
}

// policyRoutes are the routes of one actor's policy: the routes of every actor, and egress when it is set.
func policyRoutes(routes []GatewayRoute, egress *GatewayRoute) []GatewayRoute {
	all := slices.Clone(routes)
	if egress != nil {
		all = append(all, *egress)
	}
	return all
}

// gatewayRules renders the rules: one HTTP rule for each gateway listener (host and port), with the actor-token
// effect of that listener (issue #1489). Substrate allows two rules on one host when their ports differ, and the
// exact port makes a rule win for its port; only the winning rule's effects apply, so each listener gets the JWT
// of its own audience. Rules are sorted by host, then by port, so the same routes give the same rules.
func gatewayRules(routes []GatewayRoute, egress *GatewayRoute) []*ateapiv1.EgressRule {
	all := policyRoutes(routes, egress)
	slices.SortStableFunc(all, func(a, b GatewayRoute) int {
		return cmp.Or(cmp.Compare(a.Host, b.Host), cmp.Compare(a.Port, b.Port))
	})
	all = slices.CompactFunc(all, GatewayRoute.sameListener)
	rules := make([]*ateapiv1.EgressRule, 0, len(all))
	for _, r := range all {
		rules = append(rules, &ateapiv1.EgressRule{Http: &ateapiv1.HttpRule{
			Hostnames: []string{r.Host},
			Ports:     &ateapiv1.Ports{Numbers: []int32{int32(r.Port)}}, //nolint:gosec // ValidateGatewayRoutes keeps the port in 1-65535
			Effects:   actorTokenEffects(r),
		}})
	}
	return rules
}

// checkActorTokenEffects refuses rules where an actor JWT can reach anything other than a Koinon gateway listener,
// or a listener with the JWT of another audience (issues #1386 and #1489; lab test of 2026-11-27). atenet-egress
// replaces the header on an HttpsRule (MITM) too, and then the upstream host, or the guest through an echo, sees
// the token. So the effect is allowed only on the clear-text HttpRule of a gateway listener, with its one host,
// its one port and exactly the effect of that listener. Every other rule, of any kind, must carry no
// replace_headers at all.
func checkActorTokenEffects(rules []*ateapiv1.EgressRule, routes []GatewayRoute) error {
	seen := map[int]bool{}
	for i, r := range rules {
		switch {
		case r.GetHttp() != nil && r.GetHttps() != nil:
			// The API says one rule is one union member; a rule with both is not a rule this driver wrote
			// (issue #1485).
			return fmt.Errorf("rule %d: a rule has both http and https; the actor JWT may go only to a gateway listener", i)
		case len(r.GetHttps().GetEffects().GetReplaceHeaders()) > 0:
			return fmt.Errorf("rule %d: an https rule carries replace_headers; the actor JWT may go only to a gateway listener", i)
		case len(r.GetHttp().GetEffects().GetReplaceHeaders()) == 0:
			continue
		}
		h := r.GetHttp()
		at := -1
		if len(h.GetHostnames()) == 1 && h.GetPorts().GetAll() == nil && len(h.GetPorts().GetNumbers()) == 1 {
			at = slices.IndexFunc(routes, func(g GatewayRoute) bool {
				return g.Host == h.GetHostnames()[0] && int32(g.Port) == h.GetPorts().GetNumbers()[0] //nolint:gosec // ValidateGatewayRoutes keeps the port in 1-65535
			})
		}
		if at < 0 {
			return fmt.Errorf("rule %d (%v): replace_headers on a rule that is not one gateway listener", i, h.GetHostnames())
		}
		if seen[at] {
			return fmt.Errorf("rule %d (%v): a second rule for gateway listener %s", i, h.GetHostnames(), routes[at])
		}
		seen[at] = true
		if !proto.Equal(h.GetEffects(), actorTokenEffects(routes[at])) {
			return fmt.Errorf("rule %d: the replace_headers of gateway listener %s are not its own (header, prefix or audience)", i, routes[at])
		}
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

// egressRoute is the egress listener route of the actor's policy: the install's one when the access has
// CodeHostEgress, else nil.
func (d *Driver) egressRoute(k key) *GatewayRoute {
	if d.cfg.CodeHostEgressRoute != nil && d.accessOf(k).CodeHostEgress {
		return d.cfg.CodeHostEgressRoute
	}
	return nil
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
	egress := d.egressRoute(k) // once: the rules and the check below use the same route
	want := &ateapiv1.EgressPolicy{
		Metadata: &ateapiv1.ResourceMetadata{Atespace: k.atespace, Name: egressPolicyName},
		Rules:    gatewayRules(d.cfg.GatewayRoutes, egress), // the routes of every actor, and the egress listener when egress is set
	}
	if err := checkActorTokenEffects(want.GetRules(), policyRoutes(d.cfg.GatewayRoutes, egress)); err != nil {
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
