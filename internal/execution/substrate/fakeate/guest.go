// SPDX-License-Identifier: Apache-2.0

package fakeate

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	ateenvv1alpha "github.com/stateracloud/koinon/internal/gen/substrate/ateenv/v1alpha"
)

// outputChunk is the most bytes one output message carries, small so that a large output takes many
// messages and a drop falls between them.
const outputChunk = 16 << 10

// fproc is a process in a guest.
type fproc struct {
	id      string
	argv    []string
	mu      sync.Mutex
	changed chan struct{} // closed and replaced on every change
	stdout  []byte
	stderr  []byte
	stdin   []byte
	inClose bool
	exited  bool
	code    int32
	started time.Time
	// lag, when set, delays the published exit: the stdin pipe closes at exit (pipeGone) and exited
	// follows lag later, as in the real guest's reaper (LagNextExits).
	lag      time.Duration
	pipeGone bool
}

func (p *fproc) update(f func()) {
	p.mu.Lock()
	f()
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
}

func (p *fproc) exit(code int32) {
	if p.lag > 0 {
		p.mu.Lock()
		gone := p.pipeGone
		p.pipeGone = true
		p.mu.Unlock()
		if gone {
			return
		}
		time.AfterFunc(p.lag, func() { p.publishExit(code) })
		return
	}
	p.publishExit(code)
}

func (p *fproc) publishExit(code int32) {
	p.update(func() {
		if !p.exited {
			p.exited, p.code = true, code
		}
	})
}

func (p *fproc) pb() *ateenvv1alpha.Process {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := ateenvv1alpha.ProcessState_PROCESS_STATE_RUNNING
	if p.exited {
		st = ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED
	}
	return &ateenvv1alpha.Process{ProcessId: p.id, Command: p.argv, State: st, ExitCode: p.code, StartedAt: timestamppb.New(p.started)}
}

type guestServer struct {
	ateenvv1alpha.UnimplementedProcessServiceServer
	ateenvv1alpha.UnimplementedFileSystemServiceServer // the real guest has none either (#1478)
	c                                                  *Cluster
}

// target resolves the router header to a running actor. A suspended actor is resumed, as the real
// router does for any request; an actor that is still booting has no guest daemon yet.
func (g *guestServer) target(ctx context.Context, method string) (*actor, error) {
	g.c.count(method)
	md, _ := metadata.FromIncomingContext(ctx)
	v := md.Get("ate-target-actor")
	if len(v) != 1 {
		return nil, status.Error(codes.InvalidArgument, "the ate-target-actor header is missing")
	}
	as, name, _ := strings.Cut(v[0], "/")
	k := key{as, name}
	g.c.mu.Lock()
	dl := g.c.delays[k]
	if dl != nil && !dl.taken && dl.method == method {
		dl.taken = true
	} else {
		dl = nil
	}
	g.c.mu.Unlock()
	if dl != nil { // on the wire: the client's cancel does not stop it (DelayNextRouterRequest)
		close(dl.held)
		select {
		case <-dl.release:
		case <-g.c.stop:
		}
		defer close(dl.landed) // after the state below is applied
	}
	g.c.mu.Lock()
	defer g.c.mu.Unlock()
	a, ok := g.c.actors[k]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no actor %s", v[0])
	}
	if a.routerRefuse { // the router answers itself, as its ext_proc does for a failure of its own
		return nil, status.Errorf(codes.Unavailable, "actor %s unavailable", v[0])
	}
	switch a.state {
	case ateapiv1.ActorState_ACTOR_STATE_RUNNING:
		if a.frontDead && a.frontHang { // the guest accepts the call and never answers: the caller's deadline ends it
			g.c.mu.Unlock()
			<-ctx.Done()
			g.c.mu.Lock()
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		if a.frontDead { // pid 1 exited: the actor still runs and nothing answers
			// The real router's dataplane (Envoy) answers a failed upstream CONNECT to the worker's atunnel
			// with this local reply, and a gRPC request gets it as Unavailable.
			return nil, status.Error(codes.Unavailable, FrontDownMessage)
		}
	case ateapiv1.ActorState_ACTOR_STATE_SUSPENDED:
		g.c.resumes[k]++
		a.state = ateapiv1.ActorState_ACTOR_STATE_RUNNING
	default:
		return nil, status.Errorf(codes.Unavailable, "actor %s is %s", v[0], a.state)
	}
	return a, nil
}

func (g *guestServer) StartProcess(ctx context.Context, r *ateenvv1alpha.StartProcessRequest) (*ateenvv1alpha.Process, error) {
	a, err := g.target(ctx, "StartProcess")
	if err != nil {
		return nil, err
	}
	if len(r.GetCommand()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "command is empty")
	}
	g.c.mu.Lock()
	a.nextPID++
	argv := r.GetCommand()
	a.started = append(a.started, append([]string(nil), argv...))
	if path.Base(argv[0]) == "koinon-guest-launcher" {
		// The launcher hardens the process, then runs the command after "--".
		i := slices.Index(argv, "--")
		if i < 0 || i == len(argv)-1 {
			g.c.mu.Unlock()
			return nil, status.Error(codes.InvalidArgument, "launcher: no command after --")
		}
		argv = argv[i+1:]
	}
	p := &fproc{id: "p" + itoa(a.nextPID), argv: argv, changed: make(chan struct{}), started: time.Now()}
	a.procs[p.id] = p
	g.c.mu.Unlock()
	a.smu.Lock()
	if a.lagExits > 0 {
		a.lagExits--
		p.lag = a.exitLag
	}
	a.smu.Unlock()
	if d := r.GetTimeout().AsDuration(); d > 0 {
		time.AfterFunc(d, func() { p.exit(137) })
	}
	go g.run(a, p)
	return p.pb(), nil
}

// run is the guest's program table.
func (g *guestServer) run(a *actor, p *fproc) {
	args := p.argv[1:]
	switch p.argv[0] {
	case "cat":
		var off int
		for {
			p.mu.Lock()
			data, closed, exited, ch := append([]byte(nil), p.stdin[off:]...), p.inClose, p.exited, p.changed
			p.mu.Unlock()
			if exited {
				return
			}
			if len(data) > 0 {
				off += len(data)
				p.update(func() { p.stdout = append(p.stdout, data...) })
				continue
			}
			if closed {
				p.exit(0)
				return
			}
			<-ch
		}
	case "mkdir":
		p.exit(0)
	case "rm":
		g.c.mu.Lock()
		for _, f := range args {
			delete(a.files, f)
		}
		g.c.mu.Unlock()
		p.exit(0)
	case "tar":
		if slices.Contains(args, "-cf") {
			g.tarc(a, p, args)
			return
		}
		g.untar(a, p, args)
	default:
		p.update(func() { p.stderr = append(p.stderr, []byte(p.argv[0]+": not found\n")...) })
		p.exit(127)
	}
}

// tarc is `tar -C DIR [--exclude=...] -cf - .`: every file below DIR, as "./name" entries, on stdout.
// It leaves out nothing: the fake home holds no cache, and Koinon's writer applies its own list.
func (g *guestServer) tarc(a *actor, p *fproc, args []string) {
	dir := "/"
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-C" {
			dir = args[i+1]
		}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	g.c.mu.Lock()
	names := make([]string, 0, len(a.files))
	for name := range a.files {
		if rel, ok := strings.CutPrefix(name, strings.TrimSuffix(dir, "/")+"/"); ok {
			names = append(names, rel)
		}
	}
	sort.Strings(names)
	for _, rel := range names {
		body := a.files[path.Join(dir, rel)]
		_ = tw.WriteHeader(&tar.Header{Name: "./" + rel, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		_, _ = tw.Write(body)
	}
	g.c.mu.Unlock()
	_ = tw.Close()
	p.update(func() { p.stdout = append(p.stdout, buf.Bytes()...) })
	p.exit(0)
}

// untar is `tar -x -f - -C DIR`: the archive is the standard input, and tar waits for its end.
func (g *guestServer) untar(a *actor, p *fproc, args []string) {
	var file, dir string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-f":
			file = args[i+1]
		case "-C":
			dir = args[i+1]
		}
	}
	fail := func(msg string) {
		p.update(func() { p.stderr = append(p.stderr, []byte("tar: "+msg+"\n")...) })
		p.exit(2)
	}
	if file != "-" {
		fail(file + ": only standard input is supported")
		return
	}
	var data []byte
	for {
		p.mu.Lock()
		closed, exited, ch := p.inClose, p.exited, p.changed
		data = append([]byte(nil), p.stdin...)
		p.mu.Unlock()
		if exited {
			return // killed
		}
		if closed {
			break
		}
		<-ch
	}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fail(err.Error())
			return
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			fail(err.Error())
			return
		}
		g.c.mu.Lock()
		a.files[path.Join(dir, h.Name)] = body //nolint:gosec // an in-memory map key, nothing is written to disk
		g.c.mu.Unlock()
	}
	p.exit(0)
}

func (g *guestServer) proc(a *actor, id string) (*fproc, error) {
	g.c.mu.Lock()
	defer g.c.mu.Unlock()
	p, ok := a.procs[id]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "process %q not found", id)
	}
	return p, nil
}

func (g *guestServer) GetProcess(ctx context.Context, r *ateenvv1alpha.GetProcessRequest) (*ateenvv1alpha.Process, error) {
	a, err := g.target(ctx, "GetProcess")
	if err != nil {
		return nil, err
	}
	p, err := g.proc(a, r.GetProcessId())
	if err != nil {
		return nil, err
	}
	return p.pb(), nil
}

func (g *guestServer) SignalProcess(ctx context.Context, r *ateenvv1alpha.SignalProcessRequest) (*ateenvv1alpha.Process, error) {
	a, err := g.target(ctx, "SignalProcess")
	if err != nil {
		return nil, err
	}
	p, err := g.proc(a, r.GetProcessId())
	if err != nil {
		return nil, err
	}
	p.exit(128 + int32(r.GetSignal()))
	return p.pb(), nil
}

func (g *guestServer) WriteProcessInput(stream ateenvv1alpha.ProcessService_WriteProcessInputServer) error {
	a, err := g.target(stream.Context(), "WriteProcessInput")
	if err != nil {
		return err
	}
	var (
		p     *fproc
		total int64
	)
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if p == nil {
			if p, err = g.proc(a, req.GetProcessId()); err != nil {
				return err
			}
		}
		p.mu.Lock()
		exited, closed, gone := p.exited, p.inClose, p.pipeGone
		p.mu.Unlock()
		switch {
		case exited:
			return status.Errorf(codes.FailedPrecondition, "process %q has exited", p.id)
		case closed:
			return status.Errorf(codes.FailedPrecondition, "stdin of process %q is closed", p.id)
		case gone && len(req.GetData()) > 0:
			// The real guest between cmd.Wait and its mark of the exit: the pipe is closed already.
			return status.Error(codes.Internal, "writing stdin: write |1: file already closed")
		case gone && req.GetClose():
			// Its CloseInput marks stdin closed first, then the close of the pipe fails.
			p.update(func() { p.inClose = true })
			return status.Error(codes.Internal, "closing stdin: close |1: file already closed")
		}
		if d := req.GetData(); len(d) > 0 {
			p.update(func() { p.stdin = append(p.stdin, d...) })
			total += int64(len(d))
		}
		if req.GetClose() {
			a.smu.Lock()
			fail := a.failWrite > 0 && p.argv[0] == "tar"
			if fail {
				a.failWrite--
			}
			a.smu.Unlock()
			if fail {
				return status.Error(codes.Unavailable, "the stream broke before close")
			}
			p.update(func() { p.inClose = true })
		}
	}
	if p == nil {
		return status.Error(codes.InvalidArgument, "process_id is required on the first message")
	}
	return stream.SendAndClose(&ateenvv1alpha.WriteProcessInputResponse{BytesWritten: total})
}

func (g *guestServer) StreamProcessOutput(r *ateenvv1alpha.StreamProcessOutputRequest, stream ateenvv1alpha.ProcessService_StreamProcessOutputServer) error {
	a, err := g.target(stream.Context(), "StreamProcessOutput")
	if err != nil {
		return err
	}
	a.smu.Lock()
	fail := a.failOut > 0
	if fail {
		a.failOut--
	}
	a.smu.Unlock()
	if fail {
		return status.Error(codes.Unavailable, "the guest daemon is not listening yet")
	}
	p, err := g.proc(a, r.GetProcessId())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	defer a.addStream(cancel)()
	so, se := r.GetStdoutOffset(), r.GetStderrOffset()
	for {
		p.mu.Lock()
		so, se = min(max(so, 0), int64(len(p.stdout))), min(max(se, 0), int64(len(p.stderr)))
		var out, errb []byte
		if so < int64(len(p.stdout)) {
			out = p.stdout[so:min(so+outputChunk, int64(len(p.stdout)))]
		} else if se < int64(len(p.stderr)) {
			errb = p.stderr[se:min(se+outputChunk, int64(len(p.stderr)))]
		}
		exited, ch := p.exited, p.changed
		p.mu.Unlock()

		switch {
		case len(out) > 0:
			so += int64(len(out))
			if err := stream.Send(&ateenvv1alpha.ProcessOutput{Output: &ateenvv1alpha.ProcessOutput_Stdout{Stdout: append([]byte(nil), out...)}}); err != nil {
				return err
			}
		case len(errb) > 0:
			se += int64(len(errb))
			if err := stream.Send(&ateenvv1alpha.ProcessOutput{Output: &ateenvv1alpha.ProcessOutput_Stderr{Stderr: append([]byte(nil), errb...)}}); err != nil {
				return err
			}
		case exited:
			return stream.Send(&ateenvv1alpha.ProcessOutput{Output: &ateenvv1alpha.ProcessOutput_Exit{Exit: p.pb()}})
		case !r.GetFollow():
			return nil
		default:
			select {
			case <-ch:
			case <-ctx.Done():
				if stream.Context().Err() == nil {
					return status.Error(codes.Unavailable, "connection to the actor dropped")
				}
				return ctx.Err()
			}
		}
	}
}
