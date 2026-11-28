// SPDX-License-Identifier: Apache-2.0

package fakeate

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
)

// The actor egress policy, as Substrate v1.0.1 keeps it (cmd/ateapi/internal/store/atepg): a singleton named
// "default" under an actor. The row has a foreign key to the actor with ON DELETE CASCADE, so DeleteActor removes
// it. Create on a missing actor is FailedPrecondition; Update is a full replacement with the uid and version as
// preconditions (Aborted when they do not match).

const egressPolicyName = "default"

// GatewayHost and GatewayPort are the gateway listener that specs give the driver as its gateway route (issue
// #1444): the name the chart gives the standalone gateway in a release "koinon" in namespace "koinon". EgressPort
// is the port of the gateway's egress listener on the same host, which only an actor with a code-host grant
// reaches.
const (
	GatewayHost        = "koinon-gateway.koinon.svc.cluster.local"
	GatewayPort uint32 = 8080
	EgressPort  uint32 = 8081
)

func checkPolicyRef(actor *ateapiv1.ObjectRef, name string) error {
	if actor.GetAtespace() == "" || actor.GetName() == "" {
		return status.Error(codes.InvalidArgument, "actor: atespace and name are required")
	}
	if name != egressPolicyName {
		return status.Errorf(codes.InvalidArgument, "name: must be %q", egressPolicyName)
	}
	return nil
}

// checkPolicyBody checks the parts of the admission rules that a Koinon policy can break: the metadata, and no two
// rules on one pattern and port.
func checkPolicyBody(actor *ateapiv1.ObjectRef, p *ateapiv1.EgressPolicy) error {
	md := p.GetMetadata()
	if md.GetName() != egressPolicyName {
		return status.Errorf(codes.InvalidArgument, "metadata.name: must be %q", egressPolicyName)
	}
	if md.GetAtespace() != actor.GetAtespace() {
		return status.Error(codes.InvalidArgument, "metadata.atespace: must match the actor")
	}
	seen := map[string]bool{}
	for _, r := range p.GetRules() {
		h := r.GetHttp()
		if h == nil || r.GetHttps() != nil || r.GetTlsPassthrough() != nil {
			continue // the fake checks HTTP rules only: Koinon writes no other kind
		}
		if len(h.GetHostnames()) == 0 {
			return status.Error(codes.InvalidArgument, "rules: an http rule needs a hostname")
		}
		if err := checkEffects(h.GetEffects()); err != nil {
			return err
		}
		ports := []string{"80"}
		switch {
		case h.GetPorts().GetAll() != nil && len(h.GetPorts().GetNumbers()) > 0:
			return status.Error(codes.InvalidArgument, "ports: all and numbers are exclusive")
		case h.GetPorts().GetAll() != nil:
			ports = []string{"all"}
		case len(h.GetPorts().GetNumbers()) > 16:
			return status.Error(codes.InvalidArgument, "ports: at most 16 numbers")
		case len(h.GetPorts().GetNumbers()) > 0:
			ports = nil
			for _, n := range h.GetPorts().GetNumbers() {
				if n < 1 || n > 65535 {
					return status.Errorf(codes.InvalidArgument, "ports: %d is out of range", n)
				}
				ports = append(ports, strconv.Itoa(int(n)))
			}
		}
		for _, host := range h.GetHostnames() {
			if host != strings.ToLower(host) || strings.HasSuffix(host, ".") || strings.Contains(host, ":") {
				return status.Errorf(codes.InvalidArgument, "hostnames: %q is not a lowercase DNS name", host)
			}
			for _, port := range ports {
				k := host + "/" + port
				if seen[k] {
					return status.Errorf(codes.InvalidArgument, "rules: two rules on %s", k)
				}
				seen[k] = true
			}
		}
	}
	return nil
}

func (s *controlServer) GetActorEgressPolicy(_ context.Context, r *ateapiv1.GetActorEgressPolicyRequest) (*ateapiv1.EgressPolicy, error) {
	c := s.c
	c.count("GetActorEgressPolicy")
	if err := checkPolicyRef(r.GetActor(), r.GetName()); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := key{r.GetActor().GetAtespace(), r.GetActor().GetName()}
	if a := c.actors[k]; a != nil && a.failPolicyGets > 0 {
		a.failPolicyGets--
		return nil, status.Errorf(codes.Unavailable, "actor %q: the egress policy read failed", k.name)
	}
	p, ok := c.policies[k]
	if !ok {
		return nil, notFound("egress policy", r.GetActor().GetName())
	}
	return proto.Clone(p).(*ateapiv1.EgressPolicy), nil
}

func (s *controlServer) CreateActorEgressPolicy(_ context.Context, r *ateapiv1.CreateActorEgressPolicyRequest) (*ateapiv1.EgressPolicy, error) {
	c := s.c
	c.count("CreateActorEgressPolicy")
	if err := checkPolicyRef(r.GetActor(), egressPolicyName); err != nil {
		return nil, err
	}
	if err := checkPolicyBody(r.GetActor(), r.GetEgressPolicy()); err != nil {
		return nil, err
	}
	k := key{r.GetActor().GetAtespace(), r.GetActor().GetName()}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failPolicyWrites > 0 {
		c.failPolicyWrites--
		return nil, status.Error(codes.Unavailable, "egress policy store unavailable")
	}
	if _, ok := c.actors[k]; !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "actor %q does not exist", k.name)
	}
	if _, ok := c.policies[k]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "the egress policy of %q exists", k.name)
	}
	c.polVersion++
	p := &ateapiv1.EgressPolicy{
		Metadata: &ateapiv1.ResourceMetadata{Atespace: k.atespace, Name: egressPolicyName, Uid: c.newUID("egp"), Version: c.polVersion},
		Rules:    proto.Clone(r.GetEgressPolicy()).(*ateapiv1.EgressPolicy).GetRules(),
	}
	c.policies[k] = p
	c.policyWrites[k]++
	return proto.Clone(p).(*ateapiv1.EgressPolicy), nil
}

func (s *controlServer) UpdateActorEgressPolicy(_ context.Context, r *ateapiv1.UpdateActorEgressPolicyRequest) (*ateapiv1.EgressPolicy, error) {
	c := s.c
	c.count("UpdateActorEgressPolicy")
	if err := checkPolicyRef(r.GetActor(), egressPolicyName); err != nil {
		return nil, err
	}
	in := r.GetEgressPolicy()
	if err := checkPolicyBody(r.GetActor(), in); err != nil {
		return nil, err
	}
	if in.GetMetadata().GetUid() == "" || in.GetMetadata().GetVersion() == 0 {
		return nil, status.Error(codes.InvalidArgument, "metadata: uid and version are required")
	}
	k := key{r.GetActor().GetAtespace(), r.GetActor().GetName()}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failPolicyWrites > 0 {
		c.failPolicyWrites--
		return nil, status.Error(codes.Unavailable, "egress policy store unavailable")
	}
	cur, ok := c.policies[k]
	if !ok {
		return nil, notFound("egress policy", k.name)
	}
	if in.GetMetadata().GetUid() != cur.GetMetadata().GetUid() || in.GetMetadata().GetVersion() != cur.GetMetadata().GetVersion() {
		return nil, status.Error(codes.Aborted, "the egress policy changed: read it again")
	}
	c.polVersion++
	p := &ateapiv1.EgressPolicy{
		Metadata: &ateapiv1.ResourceMetadata{Atespace: k.atespace, Name: egressPolicyName, Uid: cur.GetMetadata().GetUid(), Version: c.polVersion},
		Rules:    proto.Clone(in).(*ateapiv1.EgressPolicy).GetRules(),
	}
	c.policies[k] = p
	c.policyWrites[k]++
	return proto.Clone(p).(*ateapiv1.EgressPolicy), nil
}

func (s *controlServer) DeleteActorEgressPolicy(_ context.Context, r *ateapiv1.DeleteActorEgressPolicyRequest) (*ateapiv1.EgressPolicy, error) {
	c := s.c
	c.count("DeleteActorEgressPolicy")
	if err := checkPolicyRef(r.GetActor(), r.GetName()); err != nil {
		return nil, err
	}
	k := key{r.GetActor().GetAtespace(), r.GetActor().GetName()}
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.policies[k]
	if !ok {
		return nil, notFound("egress policy", k.name)
	}
	delete(c.policies, k)
	return p, nil
}

// checkEffects checks the effects of an HTTP rule as ateapi admits them (apivalidation/egress_policy.go and the
// ActorJwtSource tags of api/ateapi/v1/ateapi.proto, Substrate commit 20778a6d): a header name is an HTTP token;
// exactly one of credential_uri and actor_jwt is set; an actor_jwt has 1 to 16 distinct audiences of 1 to 512
// characters and a lifetime of 300 to 3600 seconds. The fake does not check the format of a credential_uri.
func checkEffects(e *ateapiv1.HttpRuleEffects) error {
	for i, ch := range e.GetReplaceHeaders() {
		at := "effects.replace_headers[" + strconv.Itoa(i) + "]"
		if !validHeaderName(ch.GetHeader()) {
			return status.Errorf(codes.InvalidArgument, "%s.header: must be an HTTP header name", at)
		}
		jwt := ch.GetActorJwt()
		switch {
		case (ch.GetCredentialUri() != "") == (jwt != nil):
			return status.Errorf(codes.InvalidArgument, "%s: exactly one of credential_uri and actor_jwt must be set", at)
		case jwt == nil:
			continue
		case len(jwt.GetAudiences()) < 1 || len(jwt.GetAudiences()) > 16:
			return status.Errorf(codes.InvalidArgument, "%s.actor_jwt.audiences: 1 to 16 items", at)
		case jwt.GetExpirationSeconds() < 300 || jwt.GetExpirationSeconds() > 3600:
			return status.Errorf(codes.InvalidArgument, "%s.actor_jwt.expiration_seconds: must be 300 to 3600", at)
		}
		seen := map[string]bool{}
		for _, a := range jwt.GetAudiences() {
			if a == "" || len(a) > 512 || seen[a] {
				return status.Errorf(codes.InvalidArgument, "%s.actor_jwt.audiences: %q is empty, longer than 512 or repeated", at, a)
			}
			seen[a] = true
		}
	}
	return nil
}

func validHeaderName(v string) bool {
	if v == "" {
		return false
	}
	for _, c := range []byte(v) {
		if ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)); !ok {
			return false
		}
	}
	return true
}

// EgressEffects returns the effects that atenet-egress applies to a request to host:port: the effects of the one
// rule that decides. Of the HTTP rules that match, the winner has an exact hostname over a wildcard, then an exact
// port number over all ports ("Only the winning rule's effects are applied", api/ateapi/v1/ateapi.proto). It
// returns nil when no rule matches or the winner has no effects.
func (c *Cluster) EgressEffects(atespace, actor, host string, port uint32) *ateapiv1.HttpRuleEffects {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.policies[key{atespace, actor}]
	if !ok {
		return nil
	}
	var best *ateapiv1.HttpRule
	bestScore := -1
	for _, r := range p.GetRules() {
		h := r.GetHttp()
		if h == nil || !portIn(h.GetPorts(), port) {
			continue
		}
		for _, pat := range h.GetHostnames() {
			if !hostMatch(pat, host) {
				continue
			}
			score := 0
			if !strings.Contains(pat, "*") {
				score += 2
			}
			if h.GetPorts().GetAll() == nil {
				score++
			}
			if score > bestScore {
				best, bestScore = h, score
			}
		}
	}
	return proto.Clone(best.GetEffects()).(*ateapiv1.HttpRuleEffects)
}

// EgressPolicy is the egress policy of an actor, or nil when it has none.
func (c *Cluster) EgressPolicy(atespace, actor string) *ateapiv1.EgressPolicy {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.policies[key{atespace, actor}]
	if !ok {
		return nil
	}
	return proto.Clone(p).(*ateapiv1.EgressPolicy)
}

// PolicyWrites is how many times an actor's egress policy was created or updated.
func (c *Cluster) PolicyWrites(atespace, actor string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.policyWrites[key{atespace, actor}]
}

// SetEgressPolicy writes an actor's egress policy as another writer would (rules nil deletes it). The actor must
// exist.
func (c *Cluster) SetEgressPolicy(atespace, actor string, rules []*ateapiv1.EgressRule) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := key{atespace, actor}
	if rules == nil {
		delete(c.policies, k)
		return
	}
	c.polVersion++
	uid := c.newUID("egp")
	if cur, ok := c.policies[k]; ok {
		uid = cur.GetMetadata().GetUid()
	}
	c.policies[k] = &ateapiv1.EgressPolicy{
		Metadata: &ateapiv1.ResourceMetadata{Atespace: atespace, Name: egressPolicyName, Uid: uid, Version: c.polVersion},
		Rules:    rules,
	}
}

// FailPolicyWrites makes the next n creates or updates of an egress policy fail with Unavailable.
func (c *Cluster) FailPolicyWrites(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failPolicyWrites = n
}

// EgressAllowed reports whether the actor's egress policy lets cleartext HTTP to host:port out, as atenet-egress
// decides it for an HTTP rule: the request's host must match a hostname pattern (exact, "*." for one leftmost
// label, or "*") and the port the guest dialed must be in the rule's ports (80 when unset). An actor without a
// policy reaches nothing. Only HTTP rules count: Koinon writes no other kind.
func (c *Cluster) EgressAllowed(atespace, actor, host string, port uint32) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.policies[key{atespace, actor}]
	if !ok {
		return false
	}
	for _, r := range p.GetRules() {
		h := r.GetHttp()
		if h == nil {
			continue
		}
		if !portIn(h.GetPorts(), port) {
			continue
		}
		if slices.ContainsFunc(h.GetHostnames(), func(pat string) bool { return hostMatch(pat, host) }) {
			return true
		}
	}
	return false
}

func portIn(p *ateapiv1.Ports, port uint32) bool {
	switch {
	case p.GetAll() != nil:
		return true
	case len(p.GetNumbers()) == 0:
		return port == 80
	}
	return slices.Contains(p.GetNumbers(), int32(port)) //nolint:gosec // a port fits in int32
}

func hostMatch(pattern, host string) bool {
	switch {
	case pattern == "*":
		return true
	case strings.HasPrefix(pattern, "*."):
		label, rest, ok := strings.Cut(host, ".")
		return ok && label != "" && "*."+rest == pattern
	}
	return pattern == host
}
