// SPDX-License-Identifier: Apache-2.0

package fakeate

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"path"
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
}

func (p *fproc) update(f func()) {
	p.mu.Lock()
	f()
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
}

func (p *fproc) exit(code int32) {
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
	ateenvv1alpha.UnimplementedFileSystemServiceServer
	c *Cluster
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
	defer g.c.mu.Unlock()
	a, ok := g.c.actors[k]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no actor %s", v[0])
	}
	switch a.state {
	case ateapiv1.ActorState_ACTOR_STATE_RUNNING:
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
	p := &fproc{id: "p" + itoa(a.nextPID), argv: r.GetCommand(), changed: make(chan struct{}), started: time.Now()}
	a.procs[p.id] = p
	g.c.mu.Unlock()
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
		g.untar(a, p, args)
	default:
		p.update(func() { p.stderr = append(p.stderr, []byte(p.argv[0]+": not found\n")...) })
		p.exit(127)
	}
}

// untar is `tar -x -f FILE -C DIR`.
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
	g.c.mu.Lock()
	data, ok := a.files[file]
	g.c.mu.Unlock()
	fail := func(msg string) {
		p.update(func() { p.stderr = append(p.stderr, []byte("tar: "+msg+"\n")...) })
		p.exit(2)
	}
	if !ok {
		fail(file + ": no such file")
		return
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
		exited, closed := p.exited, p.inClose
		p.mu.Unlock()
		switch {
		case exited:
			return status.Errorf(codes.FailedPrecondition, "process %q has exited", p.id)
		case closed:
			return status.Errorf(codes.FailedPrecondition, "stdin of process %q is closed", p.id)
		}
		if d := req.GetData(); len(d) > 0 {
			p.update(func() { p.stdin = append(p.stdin, d...) })
			total += int64(len(d))
		}
		if req.GetClose() {
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

func (g *guestServer) WriteFile(stream ateenvv1alpha.FileSystemService_WriteFileServer) error {
	a, err := g.target(stream.Context(), "WriteFile")
	if err != nil {
		return err
	}
	var (
		name string
		buf  []byte
	)
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err // a cancelled stream stores nothing
		}
		if name == "" {
			if name = req.GetPath(); name == "" {
				return status.Error(codes.InvalidArgument, "file path cannot be empty")
			}
		}
		buf = append(buf, req.GetChunk()...)
	}
	if name == "" {
		return status.Error(codes.InvalidArgument, "no file data received")
	}
	g.c.mu.Lock()
	a.files[path.Clean(name)] = buf
	g.c.mu.Unlock()
	return stream.SendAndClose(&ateenvv1alpha.WriteFileResponse{BytesWritten: int64(len(buf))})
}

func (g *guestServer) ReadFile(r *ateenvv1alpha.ReadFileRequest, stream ateenvv1alpha.FileSystemService_ReadFileServer) error {
	a, err := g.target(stream.Context(), "ReadFile")
	if err != nil {
		return err
	}
	g.c.mu.Lock()
	b, ok := a.files[path.Clean(r.GetPath())]
	g.c.mu.Unlock()
	if !ok {
		return status.Errorf(codes.NotFound, "file %q not found", r.GetPath())
	}
	for len(b) > 0 {
		n := min(len(b), outputChunk)
		if err := stream.Send(&ateenvv1alpha.ReadFileResponse{Chunk: b[:n]}); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}
