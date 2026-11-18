// SPDX-License-Identifier: Apache-2.0

package fakeate

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
)

// actor is an actor and its guest.
type actor struct {
	meta      *ateapiv1.ResourceMetadata
	template  string
	sourceTag string
	state     ateapiv1.ActorState
	crash     string
	snapshots []*ateapiv1.Snapshot

	// the guest: it survives a suspend, as the memory of a snapshotted VM does
	procs   map[string]*fproc
	files   map[string][]byte
	nextPID int

	smu       sync.Mutex
	failOut   int // the next output streams that fail at once with Unavailable
	failWrite int // the next WriteFile streams that end with Unavailable when the client closes
	failRes   int // the next ResumeActor calls that fail with Unavailable
	streams   map[int]context.CancelFunc
	nextSID   int
}

func (a *actor) dropStreams() {
	a.smu.Lock()
	defer a.smu.Unlock()
	for _, cancel := range a.streams {
		cancel()
	}
}

func (a *actor) addStream(cancel context.CancelFunc) (remove func()) {
	a.smu.Lock()
	defer a.smu.Unlock()
	if a.streams == nil {
		a.streams = map[int]context.CancelFunc{}
	}
	a.nextSID++
	id := a.nextSID
	a.streams[id] = cancel
	return func() {
		a.smu.Lock()
		delete(a.streams, id)
		a.smu.Unlock()
	}
}

func cloneTemplate(t *ateapiv1.ActorTemplate) *ateapiv1.ActorTemplate {
	return proto.Clone(t).(*ateapiv1.ActorTemplate)
}

// pb renders the actor. The caller holds c.mu.
func (a *actor) pb() *ateapiv1.Actor {
	st := &ateapiv1.ActorStatus{State: a.state, Snapshots: a.snapshots}
	if a.state == ateapiv1.ActorState_ACTOR_STATE_RUNNING || a.state == ateapiv1.ActorState_ACTOR_STATE_RESUMING {
		st.WorkerAssignment = &ateapiv1.WorkerAssignment{WorkerPod: "worker-0"}
	}
	if a.state == ateapiv1.ActorState_ACTOR_STATE_CRASHED {
		st.Crash = &ateapiv1.ActorCrash{Message: a.crash}
	}
	out := &ateapiv1.Actor{
		Metadata:      proto.Clone(a.meta).(*ateapiv1.ResourceMetadata),
		ActorTemplate: &ateapiv1.ObjectRef{Atespace: a.meta.Atespace, Name: a.template},
		Status:        proto.Clone(st).(*ateapiv1.ActorStatus),
	}
	if a.sourceTag != "" {
		out.SourceTag = &ateapiv1.ObjectRef{Atespace: a.meta.Atespace, Name: a.sourceTag}
	}
	return out
}

type controlServer struct {
	ateapiv1.UnimplementedControlServer
	c *Cluster
}

func notFound(what, name string) error {
	return status.Errorf(codes.NotFound, "%s %q not found", what, name)
}

func (s *controlServer) CreateAtespace(_ context.Context, r *ateapiv1.CreateAtespaceRequest) (*ateapiv1.Atespace, error) {
	c := s.c
	c.count("CreateAtespace")
	name := r.GetAtespace().GetMetadata().GetName()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.atespaces[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "atespace %q exists", name)
	}
	c.atespaces[name] = c.newUID("as")
	return &ateapiv1.Atespace{Metadata: &ateapiv1.ResourceMetadata{Name: name, Uid: c.atespaces[name]}}, nil
}

func (s *controlServer) GetAtespace(_ context.Context, r *ateapiv1.GetAtespaceRequest) (*ateapiv1.Atespace, error) {
	c := s.c
	c.count("GetAtespace")
	name := r.GetAtespace().GetName()
	c.mu.Lock()
	defer c.mu.Unlock()
	uid, ok := c.atespaces[name]
	if !ok {
		return nil, notFound("atespace", name)
	}
	return &ateapiv1.Atespace{Metadata: &ateapiv1.ResourceMetadata{Name: name, Uid: uid}}, nil
}

func (s *controlServer) CreateActorTemplate(_ context.Context, r *ateapiv1.CreateActorTemplateRequest) (*ateapiv1.ActorTemplate, error) {
	c := s.c
	c.count("CreateActorTemplate")
	t := cloneTemplate(r.GetActorTemplate())
	k := key{t.GetMetadata().GetAtespace(), t.GetMetadata().GetName()}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.atespaces[k.atespace]; !ok {
		return nil, notFound("atespace", k.atespace)
	}
	if _, ok := c.templates[k]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "template %q exists", k.name)
	}
	t.Metadata.Uid = c.newUID("tpl")
	c.templates[k] = t
	return cloneTemplate(t), nil
}

func (s *controlServer) GetActorTemplate(_ context.Context, r *ateapiv1.GetActorTemplateRequest) (*ateapiv1.ActorTemplate, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.templates[key{r.GetActorTemplate().GetAtespace(), r.GetActorTemplate().GetName()}]
	if !ok {
		return nil, notFound("template", r.GetActorTemplate().GetName())
	}
	return cloneTemplate(t), nil
}

func (s *controlServer) ListActorTemplates(_ context.Context, r *ateapiv1.ListActorTemplatesRequest) (*ateapiv1.ListActorTemplatesResponse, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	resp := &ateapiv1.ListActorTemplatesResponse{}
	for k, t := range c.templates {
		if k.atespace == r.GetAtespace() {
			resp.ActorTemplates = append(resp.ActorTemplates, cloneTemplate(t))
		}
	}
	return resp, nil
}

func (s *controlServer) CreateActor(_ context.Context, r *ateapiv1.CreateActorRequest) (*ateapiv1.Actor, error) {
	c := s.c
	c.count("CreateActor")
	in := r.GetActor()
	k := key{in.GetMetadata().GetAtespace(), in.GetMetadata().GetName()}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.atespaces[k.atespace]; !ok {
		return nil, notFound("atespace", k.atespace)
	}
	if _, ok := c.actors[k]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "actor %q exists", k.name)
	}
	tk := key{k.atespace, in.GetActorTemplate().GetName()}
	tmpl, ok := c.templates[tk]
	if !ok {
		return nil, notFound("template", tk.name)
	}
	a := &actor{
		meta:     &ateapiv1.ResourceMetadata{Atespace: k.atespace, Name: k.name, Uid: c.newUID("act"), CreateTime: timestamppb.Now()},
		template: tk.name,
		state:    ateapiv1.ActorState_ACTOR_STATE_RESUMING,
		procs:    map[string]*fproc{},
		files:    map[string][]byte{},
	}
	if src := in.GetSourceTag().GetName(); src != "" {
		tag, ok := c.tags[key{k.atespace, src}]
		if !ok {
			return nil, notFound("tag", src)
		}
		if tag.GetStatus().GetActorTemplateUid() != tmpl.GetMetadata().GetUid() {
			return nil, status.Error(codes.FailedPrecondition, "an actor can only be seeded from a tag of its own template")
		}
		a.sourceTag = src
		a.snapshots = []*ateapiv1.Snapshot{proto.Clone(tag.GetStatus().GetSnapshot()).(*ateapiv1.Snapshot)}
	}
	c.actors[k] = a
	c.bootLocked(k, a)
	return a.pb(), nil
}

// bootLocked moves a RESUMING actor to RUNNING after the latency.
func (c *Cluster) bootLocked(k key, a *actor) {
	time.AfterFunc(c.latency(), func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.actors[k] == a && a.state == ateapiv1.ActorState_ACTOR_STATE_RESUMING {
			a.state = ateapiv1.ActorState_ACTOR_STATE_RUNNING
		}
	})
}

func (s *controlServer) GetActor(ctx context.Context, r *ateapiv1.GetActorRequest) (*ateapiv1.Actor, error) {
	c := s.c
	c.count("GetActor")
	k := key{r.GetActor().GetAtespace(), r.GetActor().GetName()}
	c.mu.Lock()
	a, ok := c.actors[k]
	if !ok {
		c.mu.Unlock()
		return nil, notFound("actor", k.name)
	}
	pb := a.pb()
	h := c.holds[k]
	delete(c.holds, k)
	c.mu.Unlock()
	if h != nil { // the state is read; the reply waits (HoldNextGetActor)
		close(h.held)
		select {
		case <-h.release:
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	return pb, nil
}

func (s *controlServer) SuspendActor(_ context.Context, r *ateapiv1.SuspendActorRequest) (*ateapiv1.SuspendActorResponse, error) {
	c := s.c
	c.count("SuspendActor")
	k := key{r.GetActor().GetAtespace(), r.GetActor().GetName()}
	c.mu.Lock()
	a, ok := c.actors[k]
	if !ok {
		c.mu.Unlock()
		return nil, notFound("actor", k.name)
	}
	switch a.state {
	case ateapiv1.ActorState_ACTOR_STATE_SUSPENDED:
	case ateapiv1.ActorState_ACTOR_STATE_RUNNING:
		a.state = ateapiv1.ActorState_ACTOR_STATE_SUSPENDED
		tmpl := c.templates[key{k.atespace, a.template}]
		fid := tmpl.GetSnapshotConfig().GetPreferredFidelity()
		if fid == ateapiv1.SnapshotFidelity_SNAPSHOT_FIDELITY_UNSPECIFIED {
			fid = ateapiv1.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY
		}
		a.snapshots = append(a.snapshots, &ateapiv1.Snapshot{
			Uuid: c.newUID("snap"), Generation: int32(len(a.snapshots) + 1), //nolint:gosec // a few snapshots in a test Owner: ateapiv1.SnapshotOwner_SNAPSHOT_OWNER_ACTOR,
			Fidelity: fid, ActorTemplateUid: tmpl.GetMetadata().GetUid(),
			DurableSnapshot: &ateapiv1.SnapshotStorage{
				State:  ateapiv1.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED,
				Object: &ateapiv1.ObjectSnapshot{SnapshotUri: "fake://" + k.atespace + "/" + k.name + "/" + itoa(len(a.snapshots)+1)},
			},
		})
	default:
		st := a.state
		c.mu.Unlock()
		return nil, status.Errorf(codes.FailedPrecondition, "actor %q is %s", k.name, st)
	}
	out := a.pb()
	c.mu.Unlock()
	a.dropStreams() // the guest connection ends with the VM's pause
	return &ateapiv1.SuspendActorResponse{Actor: out}, nil
}

func (s *controlServer) ResumeActor(_ context.Context, r *ateapiv1.ResumeActorRequest) (*ateapiv1.ResumeActorResponse, error) {
	c := s.c
	c.count("ResumeActor")
	k := key{r.GetActor().GetAtespace(), r.GetActor().GetName()}
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.actors[k]
	if !ok {
		return nil, notFound("actor", k.name)
	}
	a.smu.Lock()
	failRes := a.failRes > 0
	if failRes {
		a.failRes--
	}
	a.smu.Unlock()
	if failRes {
		return nil, status.Errorf(codes.Unavailable, "actor %q: resume failed", k.name)
	}
	resumed := false
	switch a.state {
	case ateapiv1.ActorState_ACTOR_STATE_SUSPENDED:
		a.state, resumed = ateapiv1.ActorState_ACTOR_STATE_RESUMING, true
		c.bootLocked(k, a)
	case ateapiv1.ActorState_ACTOR_STATE_RUNNING, ateapiv1.ActorState_ACTOR_STATE_RESUMING:
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "actor %q is %s", k.name, a.state)
	}
	return &ateapiv1.ResumeActorResponse{Actor: a.pb(), Resumed: resumed}, nil
}

func (s *controlServer) DeleteActor(_ context.Context, r *ateapiv1.DeleteActorRequest) (*ateapiv1.Actor, error) {
	c := s.c
	c.count("DeleteActor")
	k := key{r.GetActor().GetAtespace(), r.GetActor().GetName()}
	c.mu.Lock()
	a, ok := c.actors[k]
	if !ok {
		c.mu.Unlock()
		return nil, notFound("actor", k.name)
	}
	if c.AsyncDelete {
		a.state = ateapiv1.ActorState_ACTOR_STATE_DELETING
		time.AfterFunc(c.latency(), func() {
			c.mu.Lock()
			if c.actors[k] == a {
				delete(c.actors, k)
			}
			c.mu.Unlock()
		})
	} else {
		delete(c.actors, k)
	}
	out := a.pb()
	c.mu.Unlock()
	a.dropStreams()
	return out, nil
}

func (s *controlServer) CreateTag(_ context.Context, r *ateapiv1.CreateTagRequest) (*ateapiv1.Tag, error) {
	c := s.c
	c.count("CreateTag")
	in := r.GetTag()
	k := key{in.GetMetadata().GetAtespace(), in.GetMetadata().GetName()}
	c.mu.Lock()
	defer c.mu.Unlock()
	src, ok := c.actors[key{k.atespace, in.GetSourceActor().GetName()}]
	if !ok {
		return nil, notFound("actor", in.GetSourceActor().GetName())
	}
	if src.state != ateapiv1.ActorState_ACTOR_STATE_SUSPENDED || len(src.snapshots) == 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "actor %q is %s: only a suspended actor can be tagged", src.meta.Name, src.state)
	}
	if _, ok := c.tags[k]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "tag %q exists", k.name)
	}
	snap := proto.Clone(src.snapshots[len(src.snapshots)-1]).(*ateapiv1.Snapshot)
	snap.Uuid = c.newUID("snap")
	snap.Owner = ateapiv1.SnapshotOwner_SNAPSHOT_OWNER_TAG
	snap.DurableSnapshot.Object.SnapshotUri = "fake://" + k.atespace + "/tags/" + k.name
	t := &ateapiv1.Tag{
		Metadata:    &ateapiv1.ResourceMetadata{Atespace: k.atespace, Name: k.name, Uid: c.newUID("tag"), CreateTime: timestamppb.Now()},
		Scope:       in.GetScope(),
		SourceActor: &ateapiv1.ObjectRef{Atespace: k.atespace, Name: src.meta.Name},
		Status:      &ateapiv1.TagStatus{Snapshot: snap, ActorTemplateUid: c.templates[key{k.atespace, src.template}].GetMetadata().GetUid()},
	}
	c.tags[k] = t
	return proto.Clone(t).(*ateapiv1.Tag), nil
}

func (s *controlServer) GetTag(_ context.Context, r *ateapiv1.GetTagRequest) (*ateapiv1.Tag, error) {
	c := s.c
	c.count("GetTag")
	k := key{r.GetTag().GetAtespace(), r.GetTag().GetName()}
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tags[k]
	if !ok {
		return nil, notFound("tag", k.name)
	}
	return proto.Clone(t).(*ateapiv1.Tag), nil
}
