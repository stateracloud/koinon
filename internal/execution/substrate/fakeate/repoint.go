// SPDX-License-Identifier: Apache-2.0

package fakeate

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
)

// UpdateActor changes the template of an actor (issue #1482). Like Substrate it checks the uid and the
// version, and it takes effect at the next resume. It has the trap of the lab (2026-11-27): a repoint
// that renames the container passes, and the home is then empty, because the gVisor checkpoint of the
// home is keyed by the container name.
func (s *controlServer) UpdateActor(_ context.Context, r *ateapiv1.UpdateActorRequest) (*ateapiv1.Actor, error) {
	c := s.c
	c.count("UpdateActor")
	in := r.GetActor()
	k := key{in.GetMetadata().GetAtespace(), in.GetMetadata().GetName()}
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.actors[k]
	if !ok {
		return nil, notFound("actor", k.name)
	}
	if a.refuseUpdate != codes.OK {
		code := a.refuseUpdate
		a.refuseUpdate = codes.OK
		return nil, status.Errorf(code, "actor %q: the update is refused", k.name)
	}
	if in.GetMetadata().GetUid() != a.meta.Uid || in.GetMetadata().GetVersion() != a.meta.Version {
		return nil, status.Errorf(codes.Aborted, "actor %q: uid or version does not match", k.name)
	}
	tk := key{k.atespace, in.GetActorTemplate().GetName()}
	next, ok := c.templates[tk]
	if !ok {
		return nil, notFound("template", tk.name)
	}
	prev := c.templates[key{k.atespace, a.template}]
	if !sameContainerNames(prev, next) {
		a.files = map[string][]byte{}
	}
	a.template = tk.name
	a.auth = verifierOf(next)
	a.meta.Version++
	return a.pb(), nil
}

func sameContainerNames(a, b *ateapiv1.ActorTemplate) bool {
	if len(a.GetContainers()) != len(b.GetContainers()) {
		return false
	}
	for i := range a.GetContainers() {
		if a.GetContainers()[i].GetName() != b.GetContainers()[i].GetName() {
			return false
		}
	}
	return true
}

// RefuseNextUpdateActor makes the next UpdateActor of an actor fail with code, as Substrate does for a
// repoint that it refuses (FailedPrecondition or InvalidArgument).
func (c *Cluster) RefuseNextUpdateActor(atespace, name string, code codes.Code) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a, ok := c.actors[key{atespace, name}]; ok {
		a.refuseUpdate = code
	}
}

// ListActors lists the actors of an atespace in one page.
func (s *controlServer) ListActors(_ context.Context, r *ateapiv1.ListActorsRequest) (*ateapiv1.ListActorsResponse, error) {
	c := s.c
	c.count("ListActors")
	c.mu.Lock()
	defer c.mu.Unlock()
	resp := &ateapiv1.ListActorsResponse{}
	for k, a := range c.actors {
		if r.GetAtespace() == "" || k.atespace == r.GetAtespace() {
			resp.Actors = append(resp.Actors, a.pb())
		}
	}
	return resp, nil
}

// DeleteActorTemplate deletes a template. Like Substrate it has no in-use check: an actor that names the
// template is stranded (the lab, 2026-11-27).
func (s *controlServer) DeleteActorTemplate(_ context.Context, r *ateapiv1.DeleteActorTemplateRequest) (*ateapiv1.ActorTemplate, error) {
	c := s.c
	c.count("DeleteActorTemplate")
	k := key{r.GetActorTemplate().GetAtespace(), r.GetActorTemplate().GetName()}
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.templates[k]
	if !ok {
		return nil, notFound("template", k.name)
	}
	delete(c.templates, k)
	return cloneTemplate(t), nil
}
