// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/stateracloud/koinon/internal/execution"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	ateenvv1alpha "github.com/stateracloud/koinon/internal/gen/substrate/ateenv/v1alpha"
	"github.com/stateracloud/koinon/internal/harness"
)

// stdinChunk is the most bytes one stdin message carries.
const stdinChunk = 64 << 10

// Exec implements execution.Compute over the guest's ProcessService.
//
// Output is read from the guest's spool by byte offset, and the driver counts the bytes it has
// handed on. When the stream ends before an exit message (the actor was suspended, a connection
// dropped), the driver waits until the control API says the actor runs, then reads again from the
// counted offsets, so no byte is lost or doubled. Stdin is written with one short stream per write
// and checked against the byte count the guest acknowledges. The ACP stream of an agent therefore
// survives suspend and resume (ADR 0119, proof P-A).
//
// A write that fails after it was sent is not retried, because the driver cannot tell whether the
// guest took it: it fails the write, and the caller (the ACP client) owns recovery.
func (d *Driver) Exec(ctx context.Context, workspace, sandbox string, req execution.ExecRequest) (execution.Process, error) {
	const op = "Exec"
	k := key{workspace, sandbox}
	if len(req.Command) == 0 {
		return nil, fail(op, execution.ErrInvalid, "command is empty")
	}
	command := req.Command
	if req.LoginShell {
		// A login shell sources the user's profile; "$@" passes the command through unparsed.
		command = append([]string{"bash", "-lc", `exec "$@"`, "bash"}, command...)
	}
	// startProcess puts the launcher in front, as the outermost process, so the login shell and the
	// command are both hardened.
	start := &ateenvv1alpha.StartProcessRequest{Command: command, Cwd: req.Workdir, Stdin: true}
	if req.Timeout > 0 {
		start.Timeout = durationpb.New(req.Timeout)
	}
	sctx, end, err := d.enter(ctx, op, k)
	if err != nil {
		return nil, err
	}
	started, err := d.startProcess(sctx, op, k, start)
	end()
	if err != nil {
		return nil, err
	}

	pctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	p := &proc{d: d, k: k, id: started.GetProcessId(), stderr: req.Stderr, ctx: pctx, cancel: cancel, pr: pr, pw: pw, done: make(chan struct{})}
	p.in = &stdin{p: p}
	// A cancelled ctx ends stdout: closing the write side also unblocks a pump that waits in
	// pw.Write on a reader that stopped reading. After a normal exit pw is closed already, and the
	// first close wins, so the reader still sees EOF.
	context.AfterFunc(pctx, func() { _ = pw.CloseWithError(context.Cause(pctx)) })
	d.addProc(k, p)
	go p.pump()
	return p, nil
}

// launcherPrefix returns the launcher command that goes in front of every Exec of the sandbox. A
// process that starts through the ProcessService is a child of the guest server, so the image
// ENTRYPOINT never hardens it (ADR 0119, 0120 section 4; docs/dev/guest-launcher.md). The tier comes
// from the runtime class of the sandbox's template. When it is unknown the driver refuses the
// command: an unwrapped process would run with less fencing than the tier promises.
func (d *Driver) launcherPrefix(ctx context.Context, op string, k key) ([]string, error) {
	actor, err := d.control.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: ref(k.atespace, k.actor)})
	if err != nil {
		return nil, execution.GRPCError(op, err)
	}
	tk := key{k.atespace, actor.GetActorTemplate().GetName()}
	d.mu.Lock()
	tier, ok := d.tiers[tk]
	d.mu.Unlock()
	if !ok {
		tmpl, err := d.control.GetActorTemplate(ctx, &ateapiv1.GetActorTemplateRequest{ActorTemplate: ref(tk.atespace, tk.actor)})
		if err != nil {
			return nil, execution.GRPCError(op, err)
		}
		tier = d.cfg.DefaultTier
		if class := tmpl.GetWorkerSelector().GetMatchLabels()[RuntimeClassSelector]; class != "" {
			if tier, ok = d.cfg.RuntimeClassTiers[class]; !ok {
				return nil, fail(op, execution.ErrFailedPrecondition, "sandbox %s has runtime class %q, which has no isolation tier: no launcher policy applies", k, class)
			}
		}
		if IsDerivedTemplate(tk.actor) { // a derived name is a hash of its content; any other may change
			d.mu.Lock()
			d.tiers[tk] = tier
			d.mu.Unlock()
		}
	}
	prefix, ok := harness.Descriptor{}.LauncherPrefix(tier)
	if !ok {
		return nil, fail(op, execution.ErrFailedPrecondition, "sandbox %s has no isolation tier (%q): no launcher policy applies", k, tier)
	}
	return prefix, nil
}

// startProcess starts a process, retrying while the guest daemon is not up yet: an actor runs
// before its daemon listens.
func (d *Driver) startProcess(ctx context.Context, op string, k key, req *ateenvv1alpha.StartProcessRequest) (*ateenvv1alpha.Process, error) {
	// Every guest process starts here: Exec and the commands of Upload. The tier is read after the
	// caller entered the gate, so a deleted and re-created sandbox cannot get the old tier's policy.
	prefix, err := d.launcherPrefix(ctx, op, k)
	if err != nil {
		return nil, err
	}
	req.Command = slices.Concat(prefix, req.Command)
	deadline := time.Now().Add(d.cfg.GuestReadyTimeout)
	delay := d.cfg.PollInterval
	for {
		p, err := d.process.StartProcess(route(ctx, k), req)
		if err == nil {
			return p, nil
		}
		if derr := d.routerDown(op, err); derr != nil {
			return nil, derr
		}
		if status.Code(err) != codes.Unavailable || time.Now().After(deadline) {
			return nil, execution.GRPCError(op, err)
		}
		if err := sleep(ctx, delay); err != nil {
			return nil, execution.GRPCError(op, err)
		}
		delay = min(delay*2, 8*d.cfg.PollInterval)
		if err := d.waitRunning(ctx, op, k); err != nil {
			return nil, err
		}
	}
}

// proc is a running command. It is the execution.Process of this driver.
type proc struct {
	d      *Driver
	k      key
	id     string
	stderr io.Writer
	ctx    context.Context
	cancel context.CancelFunc

	pr *io.PipeReader
	pw *io.PipeWriter
	in *stdin

	mu     sync.Mutex
	closed bool

	done chan struct{}
	code int   // set before done closes
	err  error // set before done closes
}

var _ execution.Process = (*proc)(nil)

func (p *proc) Stdin() io.WriteCloser { return p.in }
func (p *proc) Stdout() io.Reader     { return p.pr }

// Wait implements execution.Process.
func (p *proc) Wait() (int, error) {
	<-p.done
	return p.code, p.err
}

// Close implements execution.Process. It ends the stream, kills the command if it still runs and
// the actor does, and may be called again.
func (p *proc) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.cancel()
	_ = p.pr.CloseWithError(io.ErrClosedPipe) // unblocks a pump that waits on a slow reader
	<-p.done
	return nil
}

// finish records the result once, ends stdout and releases the process.
func (p *proc) finish(code int, err error) {
	select {
	case <-p.done:
		return
	default:
	}
	if err != nil {
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		if closed && p.ctx.Err() != nil {
			err = &execution.Error{Op: "Exec", Kind: execution.ErrStreamClosed, Err: errors.New("the process was closed before it exited")}
		}
		p.kill()
		_ = p.pw.CloseWithError(err)
	} else {
		_ = p.pw.Close()
	}
	p.code, p.err = code, err
	p.d.removeProc(p.k, p)
	// done closes before ctx is cancelled, so a stdin call that fails on the cancelled ctx finds
	// the process ended (see stdin.Close).
	close(p.done)
	p.cancel()
}

// kill ends the guest process of a command that is abandoned, but only when the actor runs: a
// signal through the router would resume a suspended actor.
func (p *proc) kill() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(p.ctx), 2*time.Second)
	defer cancel()
	ctx, end, err := p.d.enter(ctx, "Exec", p.k)
	if err != nil {
		return
	}
	defer end()
	_, _ = p.d.process.SignalProcess(route(ctx, p.k), &ateenvv1alpha.SignalProcessRequest{
		ProcessId: p.id, Signal: ateenvv1alpha.Signal_SIGNAL_KILL,
	})
}

// pump reads the output until the process exits, re-subscribing from the counted offsets after every
// break in the stream.
func (p *proc) pump() {
	var (
		stdoutOff, stderrOff int64
		stallSince           time.Time
	)
	for {
		if err := p.ctx.Err(); err != nil {
			p.finish(0, err)
			return
		}
		// Wait for a running actor and open the gate's registration in one step, so a suspend
		// that starts now cancels this stream instead of racing it.
		waitStart := time.Now()
		sctx, cancel, err := p.d.enterWait(p.ctx, "Exec", p.k, streamCall)
		if err != nil {
			p.finish(0, err)
			return
		}
		openedAt := time.Now()
		// Only quick, consecutive failures count as a stall. Time spent waiting for a suspended
		// actor is not one, so the clock starts again after a long wait.
		settled := 4 * p.d.cfg.PollInterval
		if openedAt.Sub(waitStart) >= settled {
			stallSince = time.Time{}
		}
		stream, err := p.d.process.StreamProcessOutput(route(sctx, p.k), &ateenvv1alpha.StreamProcessOutputRequest{
			ProcessId: p.id, StdoutOffset: stdoutOff, StderrOffset: stderrOff, Follow: true,
		})
		progress := false
		for err == nil {
			var msg *ateenvv1alpha.ProcessOutput
			if msg, err = stream.Recv(); err != nil {
				break
			}
			switch o := msg.GetOutput().(type) {
			case *ateenvv1alpha.ProcessOutput_Stdout:
				if _, werr := p.pw.Write(o.Stdout); werr != nil {
					cancel()
					if cerr := p.ctx.Err(); cerr != nil {
						werr = cerr // the write ended because ctx did
					}
					p.finish(0, werr)
					return
				}
				stdoutOff += int64(len(o.Stdout))
				progress = len(o.Stdout) > 0 || progress
			case *ateenvv1alpha.ProcessOutput_Stderr:
				if p.stderr != nil {
					_, _ = p.stderr.Write(o.Stderr)
				}
				stderrOff += int64(len(o.Stderr))
				progress = len(o.Stderr) > 0 || progress
			case *ateenvv1alpha.ProcessOutput_Exit:
				cancel()
				p.finish(int(o.Exit.GetExitCode()), nil)
				return
			}
		}
		cancel()

		if cerr := p.ctx.Err(); cerr != nil {
			p.finish(0, cerr)
			return
		}
		if status.Code(err) == codes.NotFound || status.Code(err) == codes.InvalidArgument {
			// The guest does not know the process any more (it restarted without the spool).
			p.finish(0, &execution.Error{Op: "Exec", Kind: execution.ErrStreamClosed, Err: err})
			return
		}
		// A break: the actor was suspended, or the connection dropped. Read again from the offsets,
		// once the actor runs. A stream that fails over and over without a byte is a failure.
		if progress || time.Since(openedAt) >= settled {
			// A stream that stayed up (idle between prompts) was not failing; a drop after it is a
			// new break, not a continuation of the last one.
			stallSince = time.Time{}
		} else if stallSince.IsZero() {
			stallSince = time.Now()
		} else if time.Since(stallSince) > p.d.cfg.StreamStall {
			p.finish(0, execution.GRPCError("Exec", status.Error(codes.Unavailable, "output stream keeps failing: "+err.Error())))
			return
		}
		if serr := sleep(p.ctx, p.d.cfg.PollInterval/5); serr != nil {
			p.finish(0, serr)
			return
		}
	}
}

// stdin writes the command's standard input, one short stream per write.
type stdin struct {
	p      *proc
	mu     sync.Mutex
	closed bool
}

func (s *stdin) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errors.New("substrate: stdin is closed")
	}
	for off := 0; off < len(data); off += stdinChunk {
		chunk := data[off:min(off+stdinChunk, len(data))]
		if err := s.send(&ateenvv1alpha.WriteProcessInputRequest{ProcessId: s.p.id, Data: chunk}, int64(len(chunk))); err != nil {
			return off, err
		}
	}
	return len(data), nil
}

// Close sends end-of-file. A process that has exited or closed its stdin already is not an error,
// also when the guest fails the close because the exit races it.
func (s *stdin) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	select {
	case <-s.p.done:
		return nil // the command ended; there is no input to close
	default:
	}
	err := s.send(&ateenvv1alpha.WriteProcessInputRequest{ProcessId: s.p.id, Close: true}, 0)
	if err == nil || errors.Is(err, execution.ErrFailedPrecondition) {
		return nil
	}
	// The guest can fail a close because the command exited before the guest recorded the exit. Its
	// reaper calls cmd.Wait, which closes the stdin pipe, and marks stdin closed only after it synced
	// the output spool; a close in that window gets Internal "closing stdin: close |1: file already
	// closed" (upstream env guest/process/tracker.go, reap and CloseInput). The exit message follows
	// that mark, so wait for it: a command that ended has no input to close. A command that still
	// runs after the grace keeps the error. Only Internal comes from that window; any other error
	// (Unavailable: the stream broke) returns at once. Interim until a Substrate env update closes
	// the window (issue #1549).
	if status.Code(err) != codes.Internal {
		select {
		case <-s.p.done:
			return nil // the command ended while the close was on its way (finish cancels ctx after done)
		default:
		}
		return err
	}
	t := time.NewTimer(s.p.d.exitGrace)
	defer t.Stop()
	select {
	case <-s.p.done:
		return nil // the command ended while the close was on its way
	case <-t.C:
	}
	return err
}

// stdinExitGrace bounds the wait of stdin.Close for the exit of a command whose close failed. The
// guest publishes the exit right after two fsyncs of the output spool.
const stdinExitGrace = 5 * time.Second

func (s *stdin) send(req *ateenvv1alpha.WriteProcessInputRequest, want int64) error {
	p := s.p
	ctx, end, err := p.d.enterWait(p.ctx, "Exec", p.k, shortCall)
	if err != nil {
		return err
	}
	defer end()
	stream, err := p.d.process.WriteProcessInput(route(ctx, p.k))
	if err != nil {
		return execution.GRPCError("Exec", err)
	}
	if err := stream.Send(req); err != nil && !errors.Is(err, io.EOF) {
		return execution.GRPCError("Exec", err)
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return execution.GRPCError("Exec", err)
	}
	if resp.GetBytesWritten() != want {
		return fail("Exec", execution.ErrUnavailable, "guest took %d of %d stdin bytes", resp.GetBytesWritten(), want)
	}
	return nil
}
