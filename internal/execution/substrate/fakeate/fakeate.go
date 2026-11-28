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
//   - a tag needs a suspended source actor, and an actor seeded from a tag must use the tag's template;
//   - an actor's egress policy is a singleton that goes away with the actor, and EgressAllowed decides an
//     HTTP request as atenet-egress does (issue #1444).
//
// Guest commands are Go: cat, mkdir -p, tar -x -f F -C D and rm -f F. Any other command exits 127.
// The router needs no TLS and no SPIFFE ID here.
package fakeate

import (
	"context"
	"maps"
	"net"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	ateenvv1alpha "github.com/stateracloud/koinon/internal/gen/substrate/ateenv/v1alpha"
	"github.com/stateracloud/koinon/internal/guestauth"
)

type key struct{ atespace, name string }

// Cluster is one fake Substrate.
type Cluster struct {
	// Latency is how long an actor takes to boot or resume. Zero means 1 ms.
	Latency time.Duration
	// AsyncDelete keeps a deleted actor in state DELETING for Latency before it disappears.
	AsyncDelete bool
	// Unguarded plays an image without the guest front (from before issue #1309): no token check
	// and no Bind method. Set it before Start.
	Unguarded bool

	enforce          bool     // EnforceAuthz
	bootstrap        []string // the principals that always hold global owner
	globalPolicy     *ateapiv1.AccessPolicy
	atespacePolicies map[string]*ateapiv1.AccessPolicy
	polVersion       int64

	mu               sync.Mutex
	uid              int
	atespaces        map[string]string // name to uid
	templates        map[key]*ateapiv1.ActorTemplate
	actors           map[key]*actor
	armed            map[key][2]int // FailNextResumesAfter for an actor that does not exist yet
	tags             map[key]*ateapiv1.Tag
	policies         map[key]*ateapiv1.EgressPolicy // the actor egress policies
	policyWrites     map[key]int
	failPolicyWrites int
	tagAuth          map[key]*guestauth.Verifier // the guest front's state in a tag's memory snapshot
	resumes          map[key]int
	calls            map[string]int
	holds            map[key]*getHold
	tagHolds         map[key]*getHold
	delays           map[key]*delay
	stop             chan struct{}

	ctl, rtr         *grpc.Server
	ctlLis, routerLn *bufconn.Listener
}

// New returns an empty cluster.
func New() *Cluster {
	return &Cluster{
		atespaces:        map[string]string{},
		atespacePolicies: map[string]*ateapiv1.AccessPolicy{},
		templates:        map[key]*ateapiv1.ActorTemplate{},
		actors:           map[key]*actor{},
		tags:             map[key]*ateapiv1.Tag{},
		policies:         map[key]*ateapiv1.EgressPolicy{},
		policyWrites:     map[key]int{},
		tagAuth:          map[key]*guestauth.Verifier{},
		resumes:          map[key]int{},
		calls:            map[string]int{},
		holds:            map[key]*getHold{},
		tagHolds:         map[key]*getHold{},
		delays:           map[key]*delay{},
		stop:             make(chan struct{}),
	}
}

// Start serves the control API and the router on in-memory listeners and returns a connection to
// each. Stop shuts both down.
func (c *Cluster) Start() (control, router *grpc.ClientConn, err error) {
	c.ctlLis, c.routerLn = bufconn.Listen(1<<20), bufconn.Listen(1<<20)
	c.ctl = grpc.NewServer(grpc.UnaryInterceptor(c.checkControlUnary))
	if c.Unguarded {
		c.rtr = grpc.NewServer()
	} else {
		c.rtr = grpc.NewServer(grpc.UnaryInterceptor(c.checkUnary), grpc.StreamInterceptor(c.checkStream))
	}
	ateapiv1.RegisterControlServer(c.ctl, &controlServer{c: c})
	g := &guestServer{c: c}
	ateenvv1alpha.RegisterProcessServiceServer(c.rtr, g)
	ateenvv1alpha.RegisterFileSystemServiceServer(c.rtr, g)
	if !c.Unguarded {
		c.rtr.RegisterService(&frontService, g)
	}
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
	c.mu.Lock()
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
	c.mu.Unlock()
	if c.ctl != nil {
		c.ctl.Stop()
	}
	if c.rtr != nil {
		c.rtr.Stop()
	}
}

// StopRouter stops the router server only: the control API keeps running. It models a router outage.
func (c *Cluster) StopRouter() {
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

// getHold holds back the reply of one GetActor.
type getHold struct{ held, release chan struct{} }

// HoldNextGetActor makes the next GetActor of an actor read the state and then hold its reply until
// release is called, as a slow reply does: the caller gets a state that may be stale by then. held
// closes once the state is read. release may be called more than once.
func (c *Cluster) HoldNextGetActor(atespace, name string) (held <-chan struct{}, release func()) {
	h := &getHold{held: make(chan struct{}), release: make(chan struct{})}
	c.mu.Lock()
	c.holds[key{atespace, name}] = h
	c.mu.Unlock()
	var once sync.Once
	return h.held, func() { once.Do(func() { close(h.release) }) }
}

// HoldNextCreateTag makes the next CreateTag of a source actor wait before it tags, until release
// is called. held closes once the call arrived. release may be called more than once.
func (c *Cluster) HoldNextCreateTag(atespace, source string) (held <-chan struct{}, release func()) {
	h := &getHold{held: make(chan struct{}), release: make(chan struct{})}
	c.mu.Lock()
	c.tagHolds[key{atespace, source}] = h
	c.mu.Unlock()
	var once sync.Once
	return h.held, func() { once.Do(func() { close(h.release) }) }
}

// FailGetActorsAfterSuspend makes the n GetActor calls of an actor that follow its next
// SuspendActor fail with Unavailable, as a control plane does that restarts right after the suspend.
// The suspend itself takes effect.
func (c *Cluster) FailGetActorsAfterSuspend(atespace, name string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a, ok := c.actors[key{atespace, name}]; ok {
		a.failGetsAfterSuspend = n
	}
}

// delay holds back one guest request on its way to the router (DelayNextRouterRequest).
type delay struct {
	method                string
	taken                 bool
	held, release, landed chan struct{}
}

// DelayNextRouterRequest makes the next guest request of method (for example
// "StreamProcessOutput") for an actor stop on its way to the router, as a request that is on the
// wire does: the client's cancel does not stop it. held closes once the request is held. The next
// SuspendActor of the actor lets it land once the actor is suspended, and returns only after it
// landed: the request then resumes the actor, as the real router does, though SuspendActor's reply
// says suspended. This is the late request a suspend must verify against.
func (c *Cluster) DelayNextRouterRequest(atespace, name, method string) (held <-chan struct{}) {
	d := &delay{method: method, held: make(chan struct{}), release: make(chan struct{}), landed: make(chan struct{})}
	c.mu.Lock()
	c.delays[key{atespace, name}] = d
	c.mu.Unlock()
	return d.held
}

// ResumeAfterSuspends makes each of the next n SuspendActor calls of an actor find it resumed by a
// late router request right after it suspended it: the reply says suspended, the actor runs, and
// RouterResumes counts it.
func (c *Cluster) ResumeAfterSuspends(atespace, name string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a, ok := c.actors[key{atespace, name}]; ok {
		a.lateResumes = n
	}
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

// FailNextUploadStreams makes the next n WriteProcessInput streams to a tar process end with
// Unavailable at the message that carries close, after the data arrived. The process does not get the
// close, so it stays running with its partial input, as a process does in the real guest when the
// stream that feeds it breaks (issue #1478).
func (c *Cluster) FailNextUploadStreams(atespace, name string, n int) {
	c.mu.Lock()
	a := c.actors[key{atespace, name}]
	c.mu.Unlock()
	if a != nil {
		a.smu.Lock()
		a.failWrite = n
		a.smu.Unlock()
	}
}

// Running returns the commands of the processes of an actor that have not exited, as the guest runs
// them (the launcher prefix is removed).
func (c *Cluster) Running(atespace, name string) [][]string {
	c.mu.Lock()
	a := c.actors[key{atespace, name}]
	var procs []*fproc
	if a != nil {
		for _, p := range a.procs {
			procs = append(procs, p)
		}
	}
	c.mu.Unlock()
	var out [][]string
	for _, p := range procs {
		p.mu.Lock()
		if !p.exited {
			out = append(out, slices.Clone(p.argv))
		}
		p.mu.Unlock()
	}
	return out
}

// Files returns the files that tar extracted into an actor, by path.
func (c *Cluster) Files(atespace, name string) map[string][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.actors[key{atespace, name}]
	if a == nil {
		return nil
	}
	return maps.Clone(a.files)
}

// KillFront makes the guest of a running actor answer nothing, as when pid 1 has exited: the actor
// stays RUNNING with no crash record (issue #1481). A SuspendActor of it passes and its snapshot holds
// the dead front; RevertActor brings back the front and the files of the newest snapshot.
func (c *Cluster) KillFront(atespace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.actors[key{atespace, name}]; a != nil {
		a.frontDead = true
	}
}

// FrontDownMessage is the message of the Unavailable that a guest call to a dead front gets. In the
// default upstream CONNECT mode of the router (Substrate cmd/atenet/internal/router/cmd.go, flag
// --upstream-connect), atunnel answers the CONNECT with 502 when it cannot dial the guest port
// (internal/atunnel/ingress/ingress.go, handleConnect), and Envoy answers the gRPC request with its
// upstream connect error local reply.
const FrontDownMessage = "upstream connect error or disconnect/reset before headers. reset reason: remote connection failure, transport failure reason: delayed connect error: Connection refused"

// RouterRefuse makes the router answer every guest call of the actor with an Unavailable of its own, as
// the router's ext_proc does when it cannot route the call (Substrate cmd/atenet/internal/router/ingress/
// errors.go, mapResumeError and parkingFullErr): the guest is not reached, so it says nothing about the
// front. on false ends it.
func (c *Cluster) RouterRefuse(atespace, name string, on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.actors[key{atespace, name}]; a != nil {
		a.routerRefuse = on
	}
}

// HangFront makes the dead front of KillFront take a guest call and never answer, so the caller runs into
// its own deadline. A refusing front answers Unavailable at once; this one is silent.
func (c *Cluster) HangFront(atespace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.actors[key{atespace, name}]; a != nil {
		a.frontHang = true
	}
}

// DropSnapshots removes the snapshots of an actor, as for an actor that never suspended.
func (c *Cluster) DropSnapshots(atespace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.actors[key{atespace, name}]; a != nil {
		a.snapshots, a.snapState = nil, nil
	}
}

// FailNextReverts makes the next n RevertActor calls for an actor fail with ResourceExhausted.
func (c *Cluster) FailNextReverts(atespace, name string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.actors[key{atespace, name}]; a != nil {
		a.failRevert = n
	}
}

// FailProbe makes the next n resumes of a suspended actor fail the wakeup probe: the actor ends
// CRASHED with its snapshots kept, and the call fails with Internal.
func (c *Cluster) FailProbe(atespace, name string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.actors[key{atespace, name}]; a != nil {
		a.failProbe = n
	}
}

// FailNextPolicyReads makes the next n GetActorEgressPolicy calls for an actor fail with Unavailable,
// so a driver call that writes the policy fails (issue #1481: the write after RevertActor).
func (c *Cluster) FailNextPolicyReads(atespace, name string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.actors[key{atespace, name}]; a != nil {
		a.failPolicyGets = n
	}
}

// FailPolicyReadsAfterRevert makes the n GetActorEgressPolicy calls for an actor that follow the next
// RevertActor that works fail with Unavailable: the write after the revert fails, the calls before it
// work (issue #1481).
func (c *Cluster) FailPolicyReadsAfterRevert(atespace, name string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.actors[key{atespace, name}]; a != nil {
		a.failPolicyAfterRevert = n
	}
}

// FailNextResumesAfter lets the next skip ResumeActor calls for an actor work, and then fails n of them
// with Unavailable (the resume after the first-boot seal, issue #1481).
func (c *Cluster) FailNextResumesAfter(atespace, name string, skip, n int) {
	c.mu.Lock()
	a := c.actors[key{atespace, name}]
	if a == nil { // the actor is created next: the call applies to it
		if c.armed == nil {
			c.armed = map[key][2]int{}
		}
		c.armed[key{atespace, name}] = [2]int{skip, n}
	}
	c.mu.Unlock()
	if a != nil {
		a.smu.Lock()
		a.failSkip, a.failRes = skip, n
		a.smu.Unlock()
	}
}

// PutFile writes a file into the guest of an actor, as an agent's write to its home does.
func (c *Cluster) PutFile(atespace, name, path string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.actors[key{atespace, name}]; a != nil {
		a.files[path] = body
	}
}

// FailNextResumes makes the next n ResumeActor calls for an actor fail with Unavailable and leave
// it as it was.
func (c *Cluster) FailNextResumes(atespace, name string, n int) {
	c.mu.Lock()
	a := c.actors[key{atespace, name}]
	c.mu.Unlock()
	if a != nil {
		a.smu.Lock()
		a.failRes = n
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

// Started returns every command the guest of an actor was asked to start, as the driver sent it
// (a launcher prefix included).
func (c *Cluster) Started(atespace, name string) [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.actors[key{atespace, name}]
	if !ok {
		return nil
	}
	return slices.Clone(a.started)
}
