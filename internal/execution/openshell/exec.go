// SPDX-License-Identifier: Apache-2.0

package openshell

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/stateracloud/koinon/internal/execution"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
)

// stdinChunk bounds one stdin message, well below gRPC's 4 MiB default message limit.
const stdinChunk = 32 << 10

type execStream = grpc.BidiStreamingClient[openshellv1.ExecSandboxInput, openshellv1.ExecSandboxEvent]

// Exec implements execution.Backend over ExecSandboxInteractive with tty=false, the RPC
// behind `openshell sandbox exec --no-tty` (ADR 0048, spike 7). Stdin is streamed as it is
// written and closing it sends end-of-file; output bytes pass unchanged.
//
// Exec returns once the gateway has accepted the command, so a missing or stopped sandbox
// is reported here rather than on the first read.
func (b *Backend) Exec(ctx context.Context, workspace, sandbox string, req execution.ExecRequest) (execution.Process, error) {
	const op = "Exec"
	if len(req.Command) == 0 {
		return nil, &execution.Error{Op: op, Kind: execution.ErrInvalid, Err: errors.New("exec needs a command")}
	}
	if err := b.guardSandbox(ctx, workspace, sandbox); err != nil {
		return nil, err
	}
	start := &openshellv1.ExecSandboxRequest{
		WorkspaceScope: scope(workspace),
		Sandbox:        sandbox,
		Command:        slices.Clone(req.Command),
		Workdir:        req.Workdir,
		Tty:            false,
		NoLoginShell:   !req.LoginShell,
	}
	if req.Timeout > 0 {
		start.ExecutionTimeout = durationpb.New(req.Timeout)
	}

	sctx, cancel := context.WithCancel(ctx)
	stream, err := b.c.ExecSandboxInteractive(sctx)
	if err != nil {
		cancel()
		return nil, wrap(op, err)
	}
	if err := stream.Send(&openshellv1.ExecSandboxInput{
		Payload: &openshellv1.ExecSandboxInput_Start{Start: start},
	}); err != nil {
		cancel()
		return nil, wrap(op, streamErr(stream, err))
	}
	// The gateway validates the start message and opens the relay before it sends
	// headers; a rejection ends the stream without them.
	md, err := stream.Header()
	if err != nil {
		cancel()
		return nil, wrap(op, err)
	}
	if md == nil {
		_, err := stream.Recv()
		cancel()
		if err == nil || errors.Is(err, io.EOF) {
			err = execution.ErrStreamClosed
		}
		return nil, wrap(op, err)
	}

	stdoutR, stdoutW := io.Pipe()
	p := &process{
		stream:  stream,
		cancel:  cancel,
		stdin:   &stdinWriter{stream: stream},
		stdout:  stdoutR,
		stderr:  req.Stderr,
		recvEnd: make(chan struct{}),
		done:    make(chan struct{}),
	}
	go p.receive(stdoutW)
	go func() {
		// Cancellation unblocks a pending stdout write even if nobody reads stdout.
		select {
		case <-sctx.Done():
			stdoutW.CloseWithError(wrap(op, sctx.Err()))
		case <-p.recvEnd:
		}
	}()
	return p, nil
}

// streamErr returns the stream's status when Send reports io.EOF, which only says that the
// stream has ended.
func streamErr(stream execStream, err error) error {
	if !errors.Is(err, io.EOF) {
		return err
	}
	if _, rerr := stream.Recv(); rerr != nil && !errors.Is(rerr, io.EOF) {
		return rerr
	}
	return execution.ErrStreamClosed
}

type process struct {
	stream  execStream
	cancel  context.CancelFunc
	stdin   *stdinWriter
	stdout  *io.PipeReader
	stderr  io.Writer
	recvEnd chan struct{}
	done    chan struct{}

	code int
	err  error
}

func (p *process) Stdin() io.WriteCloser { return p.stdin }
func (p *process) Stdout() io.Reader     { return p.stdout }

func (p *process) Wait() (int, error) {
	<-p.done
	return p.code, p.err
}

func (p *process) Close() error {
	p.cancel()
	<-p.done
	return nil
}

func (p *process) receive(stdout *io.PipeWriter) {
	defer close(p.done)
	exited := false
	var err error
	for {
		ev, rerr := p.stream.Recv()
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				err = wrap("Exec", rerr)
			}
			break
		}
		switch pl := ev.GetPayload().(type) {
		case *openshellv1.ExecSandboxEvent_Stdout:
			// A failed write means the stream was cancelled; Recv reports it next.
			_, _ = stdout.Write(pl.Stdout.GetData())
		case *openshellv1.ExecSandboxEvent_Stderr:
			if p.stderr != nil {
				_, _ = p.stderr.Write(pl.Stderr.GetData())
			}
		case *openshellv1.ExecSandboxEvent_Exit:
			exited = true
			p.code = int(pl.Exit.GetExitCode())
		}
	}
	switch {
	case exited:
		// The exit status is final even if the stream fails while closing.
		_ = stdout.Close()
	case err != nil:
		p.err = err
		_ = stdout.CloseWithError(err)
	default:
		p.err = &execution.Error{Op: "Exec", Kind: execution.ErrStreamClosed, Err: execution.ErrStreamClosed}
		_ = stdout.CloseWithError(p.err)
	}
	close(p.recvEnd)
	p.cancel()
}

// stdinWriter sends stdin chunks on the stream. gRPC allows one sender at a time, so
// writes and Close are serialised.
type stdinWriter struct {
	mu     sync.Mutex
	stream execStream
	closed bool
}

func (w *stdinWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	n := 0
	for len(b) > 0 {
		chunk := b[:min(len(b), stdinChunk)]
		err := w.stream.Send(&openshellv1.ExecSandboxInput{
			Payload: &openshellv1.ExecSandboxInput_Stdin{Stdin: slices.Clone(chunk)},
		})
		if err != nil {
			if errors.Is(err, io.EOF) {
				return n, io.ErrClosedPipe
			}
			return n, wrap("Exec stdin", err)
		}
		n += len(chunk)
		b = b[len(chunk):]
	}
	return n, nil
}

// Close sends end-of-file; the command keeps running and its output keeps streaming.
func (w *stdinWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return wrap("Exec stdin", w.stream.CloseSend())
}
