// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/stateracloud/koinon/internal/execution"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	"github.com/stateracloud/koinon/internal/guestauth"
)

// signedConn signs every call to the guest API (issue #1309). The guest front refuses a call
// without a token that Config.GuestSigner made for the actor in the call's target header, so agent
// code in the sandbox, which has no token, cannot use the guest API. A wrapper of the connection
// signs every call, retries included, and no call site can forget it.
type signedConn struct {
	cc grpc.ClientConnInterface
	d  *Driver
}

func (s signedConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	ctx, err := s.d.sign(ctx)
	if err != nil {
		return err
	}
	s.d.noteRouter()
	defer s.d.noteRouter()
	return s.cc.Invoke(ctx, method, args, reply, opts...)
}

func (s signedConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	ctx, err := s.d.sign(ctx)
	if err != nil {
		return nil, err
	}
	s.d.noteRouter()
	defer s.d.noteRouter()
	return s.cc.NewStream(ctx, desc, method, opts...)
}

// sign adds a token for the actor that route named. The error is a gRPC status, so the callers'
// mapping to execution errors applies.
func (d *Driver) sign(ctx context.Context) (context.Context, error) {
	if d.cfg.GuestSigner == nil {
		return nil, status.Error(codes.FailedPrecondition, "substrate: no guest API signing key (issue #1309)")
	}
	md, _ := metadata.FromOutgoingContext(ctx)
	targets := md.Get(TargetActorHeader)
	if len(targets) != 1 {
		return nil, status.Errorf(codes.Internal, "substrate: a guest API call names %d target actors, want 1", len(targets))
	}
	tok, err := d.cfg.GuestSigner.Sign(targets[0], d.lineage.prev(targets[0]))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return metadata.AppendToOutgoingContext(ctx, guestauth.Header, tok), nil
}

// lineages holds, by actor reference, the actor a sandbox was restored from. The guest front of a
// restored sandbox has its source's pin in memory, and the driver names the source in koinon_prev
// so the front moves its pin. requireRunning, which reads the actor before every guest API call,
// keeps it current; the uid tells a re-created actor of the same name from the old one.
type lineages struct {
	mu sync.Mutex
	m  map[string]lineage
}

// bound is true once a Bind call to this actor (this uid) succeeded: its guest front checks tokens
// and its pin is this actor.
type lineage struct {
	uid, prev string
	bound     bool
}

func (l *lineages) prev(actorRef string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m[actorRef].prev
}

// learn records the source of actor, reading its source tag when the actor is new to the cache.
func (d *Driver) learn(ctx context.Context, k key, actor *ateapiv1.Actor) error {
	uid := actor.GetMetadata().GetUid()
	d.lineage.mu.Lock()
	cur, ok := d.lineage.m[k.String()]
	d.lineage.mu.Unlock()
	if ok && cur.uid == uid {
		return nil
	}
	var prev string
	if src := actor.GetSourceTag(); src.GetName() != "" {
		tag, err := d.control.GetTag(ctx, &ateapiv1.GetTagRequest{Tag: src})
		switch {
		case status.Code(err) == codes.NotFound:
			// The tag is gone: the source is unknown. A front that still has the source's pin
			// refuses the call, which fails closed.
		case err != nil:
			return err
		default:
			if s := tag.GetSourceActor(); s.GetName() != "" {
				prev = s.GetAtespace() + "/" + s.GetName()
			}
		}
	}
	d.lineage.mu.Lock()
	defer d.lineage.mu.Unlock()
	d.lineage.m[k.String()] = lineage{uid: uid, prev: prev}
	return nil
}

// forgetLineage drops the cache entry of a deleted actor.
func (d *Driver) forgetLineage(k key) {
	d.lineage.mu.Lock()
	defer d.lineage.mu.Unlock()
	delete(d.lineage.m, k.String())
}

// isBound reports whether a Bind call to the actor with this uid succeeded.
func (d *Driver) isBound(k key, uid string) bool {
	d.lineage.mu.Lock()
	defer d.lineage.mu.Unlock()
	l, ok := d.lineage.m[k.String()]
	return ok && l.uid == uid && l.bound
}

// bind makes one signed call to the guest front of a running actor, so its pin is this actor's.
// requireRunning calls it once per actor before the first guest call, and SnapshotSandbox before a
// memory snapshot: a restore of the snapshot then names this actor in koinon_prev, whatever the
// actor itself was restored from. A guest without the front (an image from before issue #1309)
// answers Unimplemented and would accept unsigned calls from agent code: the driver refuses it.
// Like startProcess, it retries while the guest is not reachable yet (Unavailable): an actor that
// was just started or resumed runs before its front listens. It does not wait on the gate, because
// SnapshotSandbox calls it while it holds the gate closed.
func (d *Driver) bind(ctx context.Context, op string, k key, uid string) error {
	bctx, cancel := context.WithTimeout(ctx, d.cfg.GuestReadyTimeout)
	defer cancel()
	deadline := time.Now().Add(d.cfg.GuestReadyTimeout)
	delay := d.cfg.PollInterval
	var err error
	for {
		err = d.router.Invoke(route(bctx, k), guestauth.BindMethod, &emptypb.Empty{}, &emptypb.Empty{})
		if derr := d.routerDown(op, err); derr != nil {
			return derr
		}
		if status.Code(err) != codes.Unavailable || time.Now().After(deadline) {
			break
		}
		if sleep(bctx, delay) != nil {
			break // the deadline or the caller ended the wait: report the last guest error
		}
		delay = min(delay*2, 8*d.cfg.PollInterval)
	}
	if status.Code(err) == codes.Unimplemented {
		return fail(op, execution.ErrFailedPrecondition,
			"sandbox %s: the guest does not check guest API tokens; the image has no koinon-guest-front (issue #1309)", k)
	}
	if err != nil {
		return execution.GRPCError(op, err)
	}
	d.lineage.mu.Lock()
	defer d.lineage.mu.Unlock()
	if l, ok := d.lineage.m[k.String()]; ok && l.uid == uid {
		l.bound = true
		d.lineage.m[k.String()] = l
	}
	return nil
}
