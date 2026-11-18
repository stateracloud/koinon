// SPDX-License-Identifier: Apache-2.0

// Package fakeate is an in-process Agent Substrate for tests: the control API (ateapi), the atenet
// router and the in-guest ProcessService and FileSystemService, with the Substrate behaviour the
// driver depends on (v1.0.0, protos in third_party/substrate):
//
//   - actors boot to RUNNING after Latency and suspend synchronously; a suspend drops every open
//     guest stream and keeps the processes and files, as a memory snapshot does;
//   - any guest request for a SUSPENDED actor resumes it, and counts in RouterResumes (the proven
//     router behaviour; the driver must never cause one);
//   - a guest request for an actor that is still RESUMING fails with Unavailable (the guest daemon
//     does not listen yet);
//   - a process's output is a spool that StreamProcessOutput reads from a byte offset;
//   - a tag needs a suspended source actor, and an actor seeded from a tag must use the tag's template.
//
// Guest commands are Go: cat, mkdir -p, tar -x -f F -C D and rm -f F. Any other command exits 127.
// The router needs no TLS and no SPIFFE ID here.
package fakeate

import (
	"context"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	ateenvv1alpha "github.com/stateracloud/koinon/internal/gen/substrate/ateenv/v1alpha"
)

type key struct{ atespace, name string }

// Cluster is one fake Substrate.
type Cluster struct {
	// Latency is how long an actor takes to boot or resume. Zero means 1 ms.
	Latency time.Duration
	// AsyncDelete keeps a deleted actor in state DELETING for Latency before it disappears.
	AsyncDelete bool

	mu        sync.Mutex
	uid       int
	atespaces map[string]string // name to uid
	templates map[key]*ateapiv1.ActorTemplate
	actors    map[key]*actor
	tags      map[key]*ateapiv1.Tag
	resumes   map[key]int
	calls     map[string]int

	ctl, rtr         *grpc.Server
	ctlLis, routerLn *bufconn.Listener
}

// New returns an empty cluster.
func New() *Cluster {
	return &Cluster{
		atespaces: map[string]string{},
		templates: map[key]*ateapiv1.ActorTemplate{},
		actors:    map[key]*actor{},
		tags:      map[key]*ateapiv1.Tag{},
		resumes:   map[key]int{},
		calls:     map[string]int{},
	}
}

// Start serves the control API and the router on in-memory listeners and returns a connection to
// each. Stop shuts both down.
func (c *Cluster) Start() (control, router *grpc.ClientConn, err error) {
	c.ctlLis, c.routerLn = bufconn.Listen(1<<20), bufconn.Listen(1<<20)
	c.ctl, c.rtr = grpc.NewServer(), grpc.NewServer()
	ateapiv1.RegisterControlServer(c.ctl, &controlServer{c: c})
	g := &guestServer{c: c}
	ateenvv1alpha.RegisterProcessServiceServer(c.rtr, g)
	ateenvv1alpha.RegisterFileSystemServiceServer(c.rtr, g)
	go func() { _ = c.ctl.Serve(c.ctlLis) }()
	go func() { _ = c.rtr.Serve(c.routerLn) }()
	dial := func(l *bufconn.Listener) (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///fakeate",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return l.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	if control, err = dial(c.ctlLis); err != nil {
		return nil, nil, err
	}
	if router, err = dial(c.routerLn); err != nil {
		_ = control.Close()
		return nil, nil, err
	}
	return control, router, nil
}

// Stop stops both servers, ending open streams.
func (c *Cluster) Stop() {
	if c.ctl != nil {
		c.ctl.Stop()
	}
	if c.rtr != nil {
		c.rtr.Stop()
	}
}

func (c *Cluster) latency() time.Duration {
	if c.Latency > 0 {
		return c.Latency
	}
	return time.Millisecond
}

func (c *Cluster) newUID(prefix string) string {
	c.uid++
	return prefix + "-" + itoa(c.uid)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// RouterResumes is how many times a guest request resumed the actor. A driver that is correct
// never causes one.
func (c *Cluster) RouterResumes(atespace, actor string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resumes[key{atespace, actor}]
}

// Calls is how many times the control or guest method (for example "SuspendActor") was called.
func (c *Cluster) Calls(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[method]
}

func (c *Cluster) count(method string) {
	c.mu.Lock()
	c.calls[method]++
	c.mu.Unlock()
}

// State is the actor's state, or ACTOR_STATE_UNSPECIFIED when there is no such actor.
func (c *Cluster) State(atespace, name string) ateapiv1.ActorState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a, ok := c.actors[key{atespace, name}]; ok {
		return a.state
	}
	return ateapiv1.ActorState_ACTOR_STATE_UNSPECIFIED
}

// File returns a file in an actor's guest.
func (c *Cluster) File(atespace, name, path string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.actors[key{atespace, name}]
	if !ok {
		return nil, false
	}
	b, ok := a.files[path]
	return append([]byte(nil), b...), ok
}

// Templates returns the names of the templates of an atespace.
func (c *Cluster) Templates(atespace string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for k := range c.templates {
		if k.atespace == atespace {
			out = append(out, k.name)
		}
	}
	return out
}

// Template returns a copy of a template.
func (c *Cluster) Template(atespace, name string) (*ateapiv1.ActorTemplate, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.templates[key{atespace, name}]
	if !ok {
		return nil, false
	}
	return cloneTemplate(t), true
}

// TemplateOf is the name of the template an actor was created from.
func (c *Cluster) TemplateOf(atespace, name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a, ok := c.actors[key{atespace, name}]; ok {
		return a.template
	}
	return ""
}

// DropStreams ends every open guest stream of an actor without changing its state, as a dropped
// connection does.
func (c *Cluster) DropStreams(atespace, name string) {
	c.mu.Lock()
	a := c.actors[key{atespace, name}]
	c.mu.Unlock()
	if a != nil {
		a.dropStreams()
	}
}

// FailNextOutputStreams makes the next n output streams of an actor fail at once with Unavailable,
// as a guest daemon that is not listening yet does after a resume.
func (c *Cluster) FailNextOutputStreams(atespace, name string, n int) {
	c.mu.Lock()
	a := c.actors[key{atespace, name}]
	c.mu.Unlock()
	if a != nil {
		a.smu.Lock()
		a.failOut = n
		a.smu.Unlock()
	}
}

// TamperTag changes what the control API reports about a tag's stored snapshot, as a swapped
// object would.
func (c *Cluster) TamperTag(atespace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.tags[key{atespace, name}]; ok {
		t.Status.Snapshot.DurableSnapshot.Object.SnapshotUri += "-swapped"
	}
}

// Crash puts an actor in state CRASHED with a message.
func (c *Cluster) Crash(atespace, name, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a, ok := c.actors[key{atespace, name}]; ok {
		a.state = ateapiv1.ActorState_ACTOR_STATE_CRASHED
		a.crash = message
	}
}
