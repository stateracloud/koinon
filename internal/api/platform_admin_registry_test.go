// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"sort"
	"strings"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	guuid "github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/stateracloud/koinon/internal/db/store"
	auditv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/audit/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/audit/v1alpha1/auditv1alpha1connect"
	outwardv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/outward/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/outward/v1alpha1/outwardv1alpha1connect"
	"github.com/stateracloud/koinon/internal/services"
)

// The platform-admin registry suite (issue #1292). services.PlatformAdminRPCs lists every
// platform-scope RPC. The specs below walk the service descriptors against it, and call every listed
// RPC as an org owner, admin, member and viewer: each call must be refused with PLATFORM_ADMIN_REQUIRED
// before the handler parses or reads anything.

// platformConditionalCalls calls the RPCs that are platform-scope only for some requests, with such a
// request. Every registry entry with a condition must have a call here.
var platformConditionalCalls = map[string][]func(ctx context.Context, c *client, org string) error{
	auditv1alpha1connect.AuditServiceListProcedure: {
		func(ctx context.Context, c *client, _ string) error {
			_, err := auditv1alpha1connect.NewAuditServiceClient(c.cc()).List(ctx, &auditv1alpha1.ListRequest{Scope: auditv1alpha1.AuditScope_AUDIT_SCOPE_INSTALL})
			return err
		},
		func(ctx context.Context, c *client, _ string) error {
			_, err := auditv1alpha1connect.NewAuditServiceClient(c.cc()).List(ctx, &auditv1alpha1.ListRequest{})
			return err
		},
	},
	outwardv1alpha1connect.OutwardServiceSetOutwardTargetDisabledProcedure: {
		func(ctx context.Context, c *client, org string) error {
			_, err := c.outward.SetOutwardTargetDisabled(ctx, &outwardv1alpha1.SetOutwardTargetDisabledRequest{
				OrgId: org, Kind: outwardv1alpha1.OutwardTargetKind_OUTWARD_TARGET_KIND_PLATFORM_TRACE_DESTINATION, Disabled: true,
			})
			return err
		},
	},
}

// cc is a generic Connect client for c's session.
func (c *client) cc() *connect.Client {
	return connect.NewClient(connecthttp.NewTransport(c.b.http, c.b.e.srv.URL))
}

// koinonMethods returns every RPC of the koinon API by procedure.
func koinonMethods() map[string]protoreflect.MethodDescriptor {
	out := map[string]protoreflect.MethodDescriptor{}
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if !strings.HasPrefix(fd.Path(), "koinon/") {
			return true
		}
		svcs := fd.Services()
		for i := range svcs.Len() {
			svc := svcs.Get(i)
			ms := svc.Methods()
			for j := range ms.Len() {
				out["/"+string(svc.FullName())+"/"+string(ms.Get(j).Name())] = ms.Get(j)
			}
		}
		return true
	})
	return out
}

// looksPlatformScope is true for an RPC whose name says it is platform-scope: a method of a Platform*
// service, or a method whose name has "Platform" in it.
func looksPlatformScope(md protoreflect.MethodDescriptor) bool {
	svc := md.Parent().(protoreflect.ServiceDescriptor)
	return strings.HasPrefix(string(svc.Name()), "Platform") || strings.Contains(string(md.Name()), "Platform")
}

// platformRequest builds a request for md. With fill, it sets org_id to org and every other string
// field named id or *_id to a random UUID, so a handler that read before the check would find nothing.
func platformRequest(md protoreflect.MethodDescriptor, org string, fill bool) *dynamicpb.Message {
	req := dynamicpb.NewMessage(md.Input())
	if !fill {
		return req
	}
	fields := req.Descriptor().Fields()
	for i := range fields.Len() {
		f := fields.Get(i)
		if f.Kind() != protoreflect.StringKind || f.Cardinality() == protoreflect.Repeated {
			continue
		}
		switch name := string(f.Name()); {
		case name == "org_id":
			req.Set(f, protoreflect.ValueOfString(org))
		case name == "id" || strings.HasSuffix(name, "_id"):
			req.Set(f, protoreflect.ValueOfString(guuid.NewString()))
		}
	}
	return req
}

var _ = Describe("platform-admin registry", func() {
	It("lists only RPCs that exist, and every RPC whose name says it is platform-scope", func() {
		defined := koinonMethods()
		Expect(defined).NotTo(BeEmpty())
		registry := services.PlatformAdminRPCs()
		var problems []string
		for proc := range registry {
			if _, ok := defined[proc]; !ok {
				problems = append(problems, proc+" is in services.platformAdminRPCs but no such RPC exists")
			}
		}
		for proc, md := range defined {
			if _, ok := registry[proc]; !ok && looksPlatformScope(md) {
				problems = append(problems, proc+" looks platform-scope but is not in services.platformAdminRPCs: "+
					"call requirePlatformAdmin first in its handler and add it to the registry")
			}
		}
		for proc, cond := range registry {
			_, hasCall := platformConditionalCalls[proc]
			switch {
			case cond != "" && !hasCall:
				problems = append(problems, proc+" is platform-scope only for some requests: add those requests to platformConditionalCalls")
			case cond == "" && hasCall:
				problems = append(problems, proc+" is in platformConditionalCalls but the registry has no condition for it")
			}
		}
		sort.Strings(problems)
		Expect(problems).To(BeEmpty())
	})

	Context("against the API", func() {
		var (
			e   *env
			org string
		)
		BeforeEach(func(ctx SpecContext) {
			e = newEnv(ctx, envOptions{})
			org, _ = e.newTeamOrg(ctx)
		})

		roles := []store.OrgRole{store.OrgRoleOwner, store.OrgRoleAdmin, store.OrgRoleMember, store.OrgRoleViewer}

		expectRefused := func(err error, what string) {
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied), "%s: %v", what, err)
			reason, _ := reasonOf(err)
			Expect(reason).To(Equal("PLATFORM_ADMIN_REQUIRED"), what)
		}

		It("refuses every platform-only RPC to every org role before it reads the request", func(ctx SpecContext) {
			defined := koinonMethods()
			var procs []string
			for proc, cond := range services.PlatformAdminRPCs() {
				if cond == "" {
					procs = append(procs, proc)
				}
			}
			sort.Strings(procs)
			Expect(procs).NotTo(BeEmpty())
			for _, role := range roles {
				c := e.userIn(ctx, org, role)
				cc := c.cc()
				for _, proc := range procs {
					md := defined[proc]
					Expect(md).NotTo(BeNil(), proc)
					spec := connect.Spec{StreamType: connect.StreamTypeUnary, Schema: md, Procedure: proc}
					// An empty request (no check after validation), the caller's own org, and an org
					// and objects that do not exist (no check after a lookup).
					for _, req := range []*dynamicpb.Message{
						platformRequest(md, org, false),
						platformRequest(md, org, true),
						platformRequest(md, guuid.NewString(), true),
					} {
						err := cc.CallUnary(ctx, spec, req, dynamicpb.NewMessage(md.Output()))
						expectRefused(err, proc+" as "+string(role))
					}
				}
			}
		})

		It("refuses the platform-scope requests of the other RPCs to every org role", func(ctx SpecContext) {
			for _, role := range roles {
				c := e.userIn(ctx, org, role)
				for proc, calls := range platformConditionalCalls {
					for _, call := range calls {
						expectRefused(call(ctx, c, org), proc+" as "+string(role))
					}
				}
			}
		})
	})
})
