// SPDX-License-Identifier: Apache-2.0

package fakeate

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/stateracloud/koinon/internal/guestauth"
)

// The guest front (internal/guestfront, issue #1309): every guest call needs a token that the
// actor's verifier accepts. A refused call reaches no handler.

func (c *Cluster) checkUnary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	if err := c.checkToken(ctx); err != nil {
		return nil, err
	}
	return h(ctx, req)
}

func (c *Cluster) checkStream(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
	if err := c.checkToken(ss.Context()); err != nil {
		return err
	}
	return h(srv, ss)
}

func (c *Cluster) checkToken(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	targets := md.Get("ate-target-actor")
	if len(targets) != 1 {
		return nil // the handler refuses it
	}
	as, name, _ := strings.Cut(targets[0], "/")
	c.mu.Lock()
	a, ok := c.actors[key{as, name}]
	var v *guestauth.Verifier
	if ok {
		v = a.auth
	}
	c.mu.Unlock()
	if !ok {
		return nil // the handler answers NotFound
	}
	tokens := md.Get(guestauth.Header)
	if v == nil || len(tokens) != 1 || v.Verify(tokens[0]) != nil {
		c.count("Unauthenticated")
		return status.Error(codes.Unauthenticated, "unauthenticated")
	}
	return nil
}

// Pin is the aud that the guest front of an actor has pinned, or empty.
func (c *Cluster) Pin(atespace, name string) string {
	c.mu.Lock()
	a, ok := c.actors[key{atespace, name}]
	c.mu.Unlock()
	if !ok || a.auth == nil {
		return ""
	}
	return a.auth.Pin()
}

// bind is the front's Bind: the token check has run; the router resumes a suspended actor.
func (g *guestServer) bind(ctx context.Context) error {
	_, err := g.target(ctx, "Bind")
	return err
}

var frontService = grpc.ServiceDesc{
	ServiceName: "koinon.guestfront.v1.Front",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Bind",
		Handler: func(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
			if err := dec(new(emptypb.Empty)); err != nil {
				return nil, err
			}
			call := func(ctx context.Context, _ any) (any, error) {
				return new(emptypb.Empty), srv.(*guestServer).bind(ctx)
			}
			if ic == nil {
				return call(ctx, nil)
			}
			return ic(ctx, nil, &grpc.UnaryServerInfo{Server: srv, FullMethod: guestauth.BindMethod}, call)
		},
	}},
	Metadata: "koinon/guestfront/v1/front",
}

// testSeed is an obviously fake guest API signing key: 32 bytes of 0x11.
const testSeed = "1111111111111111111111111111111111111111111111111111111111111111"

// Signer returns a guest API signer with a fixed, obviously fake key, for a driver on this fake.
func Signer() *guestauth.Signer {
	k, err := guestauth.ParseSeed(testSeed)
	if err != nil {
		panic(err)
	}
	s, err := guestauth.NewSigner(k)
	if err != nil {
		panic(err)
	}
	return s
}
