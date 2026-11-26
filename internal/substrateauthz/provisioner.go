// SPDX-License-Identifier: Apache-2.0

package substrateauthz

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stateracloud/koinon/internal/execution"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
)

// ErrName is returned for an atespace name that is not a Koinon workspace name.
var ErrName = errors.New("not a Koinon workspace name")

var workspaceName = regexp.MustCompile("^" + regexp.QuoteMeta(execution.WorkspacePrefix) + "[0-9a-f]{16}$")

// Provisioner creates org atespaces and owns their access policy. Its principal holds the global owner role in
// Substrate, so it is the only Koinon component that does (ADR 0128). It takes no input but a workspace name of
// the form execution.WorkspaceName gives, so the caller cannot make it touch any other atespace.
type Provisioner struct {
	Control ateapiv1.ControlClient
	// Editors and Viewers are the policy members (see [Member]) that get editor and viewer in every org atespace:
	// the reconciler and agentd.
	Editors, Viewers []string
}

// EnsureAtespace creates the atespace when it is missing and writes its access policy when that is missing or
// differs. It owns the whole policy, so a full replacement is safe and it repairs drift. It is idempotent, so a
// retry after any failure, a second caller and an atespace that exists from before the provisioner are all fine.
func (p *Provisioner) EnsureAtespace(ctx context.Context, name string) error {
	if !workspaceName.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrName, name)
	}
	_, err := p.Control.CreateAtespace(ctx, &ateapiv1.CreateAtespaceRequest{
		Atespace: &ateapiv1.Atespace{Metadata: &ateapiv1.ResourceMetadata{Name: name}},
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return fmt.Errorf("create atespace %s: %w", name, err)
	}
	want := Bindings(map[string][]string{RoleEditor: p.Editors, RoleViewer: p.Viewers})
	parent := &ateapiv1.ObjectRef{Name: name}
	var last error
	for range retries {
		cur, err := p.Control.GetAtespaceAccessPolicy(ctx, &ateapiv1.GetAtespaceAccessPolicyRequest{Atespace: parent, Name: PolicyName})
		switch {
		case status.Code(err) == codes.NotFound:
			_, err = p.Control.CreateAtespaceAccessPolicy(ctx, &ateapiv1.CreateAtespaceAccessPolicyRequest{
				Atespace:     parent,
				AccessPolicy: &ateapiv1.AccessPolicy{Metadata: &ateapiv1.ResourceMetadata{Name: PolicyName}, Bindings: want},
			})
		case err != nil:
		case sameBindings(cur.GetBindings(), want):
			return nil
		default:
			_, err = p.Control.UpdateAtespaceAccessPolicy(ctx, &ateapiv1.UpdateAtespaceAccessPolicyRequest{
				Atespace: parent,
				AccessPolicy: &ateapiv1.AccessPolicy{
					Metadata: &ateapiv1.ResourceMetadata{Name: PolicyName, Uid: cur.GetMetadata().GetUid(), Version: cur.GetMetadata().GetVersion()},
					Bindings: want,
				},
			})
		}
		if err == nil {
			return nil
		}
		if !conflict(err) {
			return fmt.Errorf("write the access policy of atespace %s: %w", name, err)
		}
		last = err
	}
	return fmt.Errorf("the access policy of atespace %s kept changing: %w", name, last)
}
