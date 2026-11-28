// SPDX-License-Identifier: Apache-2.0

package fakeate

import (
	"context"
	"net"
	"slices"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
)

// EnforceAuthz makes the control API check the caller as Substrate v1.0.0's ateapi does
// (cmd/ateapi/internal/authz/model.fga): the bearer token is the principal id (a Kubernetes token's
// "system:serviceaccount:<ns>:<name>"); the bootstrap owners and the principals that the global policy binds to
// owner hold every right; a global viewer reads everything; an atespace policy binds editor and viewer. A caller
// without the right gets PermissionDenied, also for an atespace that does not exist (the check runs before the
// handler, so a missing atespace is not NotFound to a caller with no role). Set it before Start.
func (c *Cluster) EnforceAuthz(bootstrapOwners ...string) {
	c.enforce = true
	c.bootstrap = slices.Clone(bootstrapOwners)
}

// principal reads the caller's id from the bearer token.
func principal(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, v := range md.Get("authorization") {
		if t, ok := strings.CutPrefix(v, "Bearer "); ok {
			return strings.TrimSpace(t)
		}
	}
	return ""
}

// has reports whether member holds role in policy.
func has(p *ateapiv1.AccessPolicy, role, member string) bool {
	for _, b := range p.GetBindings() {
		if b.GetRole() == role && slices.Contains(b.GetMembers(), "user:"+member) {
			return true
		}
	}
	return false
}

// atespaceOf finds the atespace a request is for: an ObjectRef or a ResourceMetadata in a field of the request.
func atespaceOf(m protoreflect.Message, depth int) string {
	var found string
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.Kind() == protoreflect.StringKind && !fd.IsList() && fd.Name() == "atespace":
			found = v.String()
		case fd.Kind() == protoreflect.MessageKind && !fd.IsList() && !fd.IsMap() && depth > 0:
			found = atespaceOf(v.Message(), depth-1)
		}
		return found == ""
	})
	return found
}

// allowed applies the model to one call. The caller holds no lock.
func (c *Cluster) allowed(who, method string, req proto.Message) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	globalOwner := slices.Contains(c.bootstrap, who) || has(c.globalPolicy, "owner", who)
	if globalOwner {
		return true
	}
	globalViewer := has(c.globalPolicy, "viewer", who)
	name := method[strings.LastIndex(method, "/")+1:]
	read := strings.HasPrefix(name, "Get") || strings.HasPrefix(name, "List")
	switch {
	case strings.Contains(name, "AccessPolicy"), strings.Contains(name, "Worker"), strings.HasSuffix(name, "Atespace") && !read, name == "ListAtespaces":
		// Only a global owner writes a policy, creates or deletes an atespace or runs a worker. A global
		// viewer lists atespaces and workers.
		return read && globalViewer && !strings.Contains(name, "AccessPolicy")
	}
	as := atespaceOf(req.ProtoReflect(), 2)
	if name == "GetAtespace" {
		as = req.(*ateapiv1.GetAtespaceRequest).GetAtespace().GetName()
	}
	if as == "" {
		return read && globalViewer
	}
	pol := c.atespacePolicies[as]
	if read {
		return globalViewer || has(pol, "viewer", who) || has(pol, "editor", who)
	}
	return has(pol, "editor", who)
}

func (c *Cluster) checkControlUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	if !c.enforce {
		return h(ctx, req)
	}
	who := principal(ctx)
	if who == "" {
		return nil, status.Error(codes.Unauthenticated, "no bearer token")
	}
	m, _ := req.(proto.Message)
	if m == nil || !c.allowed(who, info.FullMethod, m) {
		return nil, status.Errorf(codes.PermissionDenied, "principal %q lacks the role for %s", "user:"+strings.ReplaceAll(who, ":", "%3A"), info.FullMethod)
	}
	return h(ctx, req)
}

// Policy is the access policy of a scope ("" is the global one), or nil.
func (c *Cluster) Policy(atespace string) *ateapiv1.AccessPolicy {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.globalPolicy
	if atespace != "" {
		p = c.atespacePolicies[atespace]
	}
	return proto.Clone(p).(*ateapiv1.AccessPolicy)
}

// SetGlobalPolicy seeds the global policy, as an operator's earlier setup would have.
func (c *Cluster) SetGlobalPolicy(bindings ...*ateapiv1.Binding) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.polVersion++
	c.globalPolicy = &ateapiv1.AccessPolicy{
		Metadata: &ateapiv1.ResourceMetadata{Name: "default", Uid: c.newUID("pol"), Version: c.polVersion},
		Bindings: bindings,
	}
}

func (s *controlServer) GetGlobalAccessPolicy(context.Context, *ateapiv1.GetGlobalAccessPolicyRequest) (*ateapiv1.AccessPolicy, error) {
	c := s.c
	c.count("GetGlobalAccessPolicy")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.globalPolicy == nil {
		return nil, notFound("access policy", "default")
	}
	return proto.Clone(c.globalPolicy).(*ateapiv1.AccessPolicy), nil
}

func (s *controlServer) CreateGlobalAccessPolicy(_ context.Context, r *ateapiv1.CreateGlobalAccessPolicyRequest) (*ateapiv1.AccessPolicy, error) {
	c := s.c
	c.count("CreateGlobalAccessPolicy")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.globalPolicy != nil {
		return nil, status.Error(codes.AlreadyExists, "the global access policy exists")
	}
	c.polVersion++
	c.globalPolicy = &ateapiv1.AccessPolicy{
		Metadata: &ateapiv1.ResourceMetadata{Name: "default", Uid: c.newUID("pol"), Version: c.polVersion},
		Bindings: r.GetAccessPolicy().GetBindings(),
	}
	return proto.Clone(c.globalPolicy).(*ateapiv1.AccessPolicy), nil
}

// replace is Substrate's full replacement with the uid and version as preconditions.
func (c *Cluster) replace(cur, in *ateapiv1.AccessPolicy) (*ateapiv1.AccessPolicy, error) {
	if cur == nil {
		return nil, notFound("access policy", "default")
	}
	if in.GetMetadata().GetUid() != cur.GetMetadata().GetUid() || in.GetMetadata().GetVersion() != cur.GetMetadata().GetVersion() {
		return nil, status.Error(codes.FailedPrecondition, "the access policy changed: read it again")
	}
	c.polVersion++
	return &ateapiv1.AccessPolicy{
		Metadata: &ateapiv1.ResourceMetadata{Atespace: cur.GetMetadata().GetAtespace(), Name: "default", Uid: cur.GetMetadata().GetUid(), Version: c.polVersion},
		Bindings: in.GetBindings(),
	}, nil
}

func (s *controlServer) UpdateGlobalAccessPolicy(_ context.Context, r *ateapiv1.UpdateGlobalAccessPolicyRequest) (*ateapiv1.AccessPolicy, error) {
	c := s.c
	c.count("UpdateGlobalAccessPolicy")
	c.mu.Lock()
	defer c.mu.Unlock()
	p, err := c.replace(c.globalPolicy, r.GetAccessPolicy())
	if err != nil {
		return nil, err
	}
	c.globalPolicy = p
	return proto.Clone(p).(*ateapiv1.AccessPolicy), nil
}

func (s *controlServer) GetAtespaceAccessPolicy(_ context.Context, r *ateapiv1.GetAtespaceAccessPolicyRequest) (*ateapiv1.AccessPolicy, error) {
	c := s.c
	c.count("GetAtespaceAccessPolicy")
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.atespacePolicies[r.GetAtespace().GetName()]
	if !ok {
		return nil, notFound("access policy", r.GetAtespace().GetName())
	}
	return proto.Clone(p).(*ateapiv1.AccessPolicy), nil
}

func (s *controlServer) CreateAtespaceAccessPolicy(_ context.Context, r *ateapiv1.CreateAtespaceAccessPolicyRequest) (*ateapiv1.AccessPolicy, error) {
	c := s.c
	c.count("CreateAtespaceAccessPolicy")
	as := r.GetAtespace().GetName()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.atespaces[as]; !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "atespace %q does not exist", as)
	}
	if _, ok := c.atespacePolicies[as]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "the access policy of %q exists", as)
	}
	c.polVersion++
	p := &ateapiv1.AccessPolicy{
		Metadata: &ateapiv1.ResourceMetadata{Name: "default", Uid: c.newUID("pol"), Version: c.polVersion},
		Bindings: r.GetAccessPolicy().GetBindings(),
	}
	c.atespacePolicies[as] = p
	return proto.Clone(p).(*ateapiv1.AccessPolicy), nil
}

func (s *controlServer) UpdateAtespaceAccessPolicy(_ context.Context, r *ateapiv1.UpdateAtespaceAccessPolicyRequest) (*ateapiv1.AccessPolicy, error) {
	c := s.c
	c.count("UpdateAtespaceAccessPolicy")
	as := r.GetAtespace().GetName()
	c.mu.Lock()
	defer c.mu.Unlock()
	p, err := c.replace(c.atespacePolicies[as], r.GetAccessPolicy())
	if err != nil {
		return nil, err
	}
	c.atespacePolicies[as] = p
	return proto.Clone(p).(*ateapiv1.AccessPolicy), nil
}

// ControlAs dials the control API as the principal who: the connection sends who as the bearer token. Close it
// when done. It is how a spec plays the reconciler, agentd or the provisioner against EnforceAuthz.
func (c *Cluster) ControlAs(who string) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///fakeate",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return c.ctlLis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, inv grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return inv(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+who), method, req, reply, cc, opts...)
		}))
}
