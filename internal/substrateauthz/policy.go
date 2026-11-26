// SPDX-License-Identifier: Apache-2.0

// Package substrateauthz is how Koinon's components get the least access they need in Agent Substrate's
// access model (ADR 0128). Substrate lets only its global owner create an atespace, so one small component,
// the provisioner, holds that role. It creates each org's atespace and writes the atespace's access policy: the
// reconciler gets editor and agentd gets viewer. The installer adds the provisioner to the global policy.
package substrateauthz

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
)

// Roles of Substrate's access model (cmd/ateapi/internal/authz/model.fga). A global policy takes owner and
// viewer; an atespace policy also takes editor.
const (
	RoleOwner  = "owner"
	RoleEditor = "editor"
	RoleViewer = "viewer"

	// PolicyName is the one name an access policy has.
	PolicyName = "default"
)

// Member is the policy member of a Kubernetes ServiceAccount, as Substrate's authenticator names the caller of a
// ServiceAccount token (docs/guides/authorization.md, Principals).
func Member(namespace, serviceAccount string) string {
	return "user:system:serviceaccount:" + namespace + ":" + serviceAccount
}

// AddMember adds member to the binding of role and reports whether the policy changed. Every other binding and
// member stays: a write replaces the whole policy, so a caller that does not own it must merge.
func AddMember(p *ateapiv1.AccessPolicy, role, member string) bool {
	for _, b := range p.Bindings {
		if b.Role != role {
			continue
		}
		if slices.Contains(b.Members, member) {
			return false
		}
		b.Members = append(b.Members, member)
		return true
	}
	p.Bindings = append(p.Bindings, &ateapiv1.Binding{Role: role, Members: []string{member}})
	return true
}

// HasMember reports whether member holds role in p.
func HasMember(p *ateapiv1.AccessPolicy, role, member string) bool {
	for _, b := range p.GetBindings() {
		if b.GetRole() == role && slices.Contains(b.GetMembers(), member) {
			return true
		}
	}
	return false
}

// Bindings builds the bindings of a policy from a role to members map, in a fixed order. A role without
// members is left out.
func Bindings(byRole map[string][]string) []*ateapiv1.Binding {
	var out []*ateapiv1.Binding
	for _, role := range []string{RoleOwner, RoleEditor, RoleViewer} {
		if m := slices.Compact(slices.Sorted(slices.Values(byRole[role]))); len(m) > 0 {
			out = append(out, &ateapiv1.Binding{Role: role, Members: m})
		}
	}
	return out
}

// sameBindings reports whether a and b grant the same members the same roles, in any order.
func sameBindings(a, b []*ateapiv1.Binding) bool {
	norm := func(in []*ateapiv1.Binding) map[string][]string {
		m := map[string][]string{}
		for _, x := range in {
			m[x.GetRole()] = append(m[x.GetRole()], x.GetMembers()...)
		}
		for r, v := range m {
			m[r] = slices.Compact(slices.Sorted(slices.Values(v)))
		}
		return m
	}
	na, nb := norm(a), norm(b)
	if len(na) != len(nb) {
		return false
	}
	for r, v := range na {
		if !slices.Equal(v, nb[r]) {
			return false
		}
	}
	return true
}

// retries bounds the read-merge-write loop on a policy that another writer changed in between.
const retries = 5

func conflict(err error) bool {
	c := status.Code(err)
	return c == codes.FailedPrecondition || c == codes.Aborted || c == codes.AlreadyExists
}

// EnsureGlobalMember makes member hold role in the global access policy and keeps every other member. It reads
// the policy, adds the member and writes it back with the uid and version it read, so a change by someone else
// in between fails the write and the step reads again. Without a policy it creates one. It reports whether it
// wrote. The installer calls it with the provisioner as owner, on install and on every upgrade.
func EnsureGlobalMember(ctx context.Context, ctl ateapiv1.ControlClient, role, member string) (bool, error) {
	if role != RoleOwner && role != RoleViewer {
		return false, fmt.Errorf("the global access policy takes owner and viewer, not %q", role)
	}
	if !strings.HasPrefix(member, "user:") {
		return false, fmt.Errorf("policy member %q must start with \"user:\"", member)
	}
	var last error
	for range retries {
		cur, err := ctl.GetGlobalAccessPolicy(ctx, &ateapiv1.GetGlobalAccessPolicyRequest{Name: PolicyName})
		if status.Code(err) == codes.NotFound {
			_, err = ctl.CreateGlobalAccessPolicy(ctx, &ateapiv1.CreateGlobalAccessPolicyRequest{AccessPolicy: &ateapiv1.AccessPolicy{
				Metadata: &ateapiv1.ResourceMetadata{Name: PolicyName},
				Bindings: []*ateapiv1.Binding{{Role: role, Members: []string{member}}},
			}})
			if err == nil {
				return true, nil
			}
		} else if err == nil {
			next := proto.Clone(cur).(*ateapiv1.AccessPolicy)
			if !AddMember(next, role, member) {
				return false, nil
			}
			_, err = ctl.UpdateGlobalAccessPolicy(ctx, &ateapiv1.UpdateGlobalAccessPolicyRequest{AccessPolicy: next})
			if err == nil {
				return true, nil
			}
		}
		if !conflict(err) {
			return false, fmt.Errorf("write the global access policy: %w", err)
		}
		last = err
	}
	return false, fmt.Errorf("the global access policy kept changing: %w", last)
}

// ComponentServiceAccount is the ServiceAccount name that the chart gives a component of release (the chart's
// koinon.componentName with the default names: the full name is the release when it holds "koinon", else
// "<release>-koinon").
func ComponentServiceAccount(release, component string) string {
	full := release
	if !strings.Contains(release, "koinon") {
		full = release + "-koinon"
	}
	if len(full) > 63 {
		full = strings.TrimSuffix(full[:63], "-")
	}
	name := full + "-" + component
	if len(name) > 63 {
		name = strings.TrimSuffix(name[:63], "-")
	}
	return name
}
