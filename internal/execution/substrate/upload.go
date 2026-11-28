// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/stateracloud/koinon/internal/execution"
	ateenvv1alpha "github.com/stateracloud/koinon/internal/gen/substrate/ateenv/v1alpha"
)

// uploadChunk is the most bytes one WriteProcessInput message carries.
const uploadChunk = 256 << 10

// uploadTimeout is the longest time the guest lets the tar of one upload run. It is a backstop: the
// driver kills the process itself when a stream breaks, but a client that dies cannot (the guest
// does not end a process when the stream that fed it ends).
const uploadTimeout = 30 * time.Minute

// Upload implements execution.Compute. The driver sends the archive on the standard input of
// `tar -x -f - -C destDir`, which runs as the payload user behind the guest launcher, after
// `mkdir -p destDir`. The commands run with no shell: the paths are arguments, never parsed. The
// guest image therefore needs mkdir and tar, which the guest launcher's image contract lists
// (epic 1082, #1087).
//
// The guest front never writes a file, so no root process writes into a path that the payload can
// write (issue #1478; the guest front has no file service). The payload user owns what tar extracts.
//
// The archive goes in one WriteProcessInput stream of uploadChunk messages. The last message carries
// close, and the driver compares the bytes that the guest took with the bytes it sent. A guest process
// does not end when the stream that feeds it breaks, so on a read error, a cancelled context or a
// break the driver sends SIGKILL to tar by its process id (when the actor runs again, if a suspend
// caused the break) and never sends close: a close would let tar finish a truncated archive. The
// router resets a stream after 300 s, so an upload that needs more time fails.
//
// A read error of the archive stops the upload and is returned in the error chain.
func (d *Driver) Upload(ctx context.Context, workspace, sandbox, destDir string, archive io.Reader) error {
	const op = "Upload"
	k := key{workspace, sandbox}
	if !strings.HasPrefix(destDir, "/") {
		return fail(op, execution.ErrInvalid, "destination %q is not absolute", destDir)
	}
	if err := d.requireRunning(ctx, op, k); err != nil {
		return err
	}
	if code, msg, err := d.runOnce(ctx, op, k, []string{"mkdir", "-p", destDir}); err != nil {
		return err
	} else if code != 0 {
		return fail(op, execution.ErrFailedPrecondition, "mkdir %s exited %d: %s", destDir, code, msg)
	}

	// A retry needs the bytes again, and archive cannot be read twice: the driver keeps the first
	// chunk, and an archive that fits in it is retried to the end. A larger archive is not retried
	// (the caller owns the stream).
	first := make([]byte, uploadChunk)
	first, complete, rerr := fillChunk(archive, first)
	if rerr != nil {
		return &execution.Error{Op: op, Err: fmt.Errorf("read archive: %w", rerr)}
	}
	if !complete {
		// fillChunk stops at a full buffer, so an archive of exactly uploadChunk bytes looks as if it had
		// more. Read one byte more: at the end it is complete and a retry is allowed (issue #1516); else
		// the byte goes back in front of the rest of the stream.
		var probe [1]byte
		n, perr := io.ReadFull(archive, probe[:])
		switch {
		case n == 0 && perr == io.EOF:
			complete = true
		case perr != nil && perr != io.EOF:
			return &execution.Error{Op: op, Err: fmt.Errorf("read archive: %w", perr)}
		default:
			archive = io.MultiReader(bytes.NewReader(probe[:n]), archive)
		}
	}
	deadline := time.Now().Add(d.cfg.GuestReadyTimeout)
	delay := d.cfg.PollInterval
	for {
		id, err := d.extract(ctx, op, k, destDir, first, complete, archive)
		if err == nil {
			return nil
		}
		retry := complete && errors.Is(err, execution.ErrUnavailable) && time.Now().Before(deadline)
		switch {
		case id == "":
		case retry:
			d.killOrphan(ctx, k, id) // the old tar must be gone before the next one starts
		default:
			go d.killOrphan(ctx, k, id) // do not make the caller wait for a suspended actor
		}
		if !retry {
			return err
		}
		if err := sleep(ctx, delay); err != nil {
			return execution.GRPCError(op, err)
		}
		delay = min(delay*2, 8*d.cfg.PollInterval)
	}
}

// extract is one start of tar and one stream of the archive into it. It returns the id of a tar that
// may still run (empty when tar ended or never started), so the caller kills it.
func (d *Driver) extract(ctx context.Context, op string, k key, destDir string, first []byte, complete bool, r io.Reader) (string, error) {
	actx, end, err := d.enter(ctx, op, k)
	if err != nil {
		return "", err
	}
	defer end()
	p, err := d.startProcess(actx, op, k, &ateenvv1alpha.StartProcessRequest{
		Command: []string{"tar", "-x", "-f", "-", "-C", destDir},
		Stdin:   true,
		Timeout: durationpb.New(uploadTimeout),
	})
	if err != nil {
		return "", err
	}
	id := p.GetProcessId()
	// When tar ends before the stream does, sendArchive returns no error and the exit code says why.
	if err := d.sendArchive(actx, op, k, id, first, complete, r); err != nil {
		return id, err
	}
	code, msg, err := d.waitExit(actx, op, k, id)
	if err != nil {
		return id, err
	}
	if code != 0 {
		return "", fail(op, execution.ErrInvalid, "tar exited %d: %s", code, msg)
	}
	return "", nil
}

// fillChunk reads r into buf until buf is full or r returns io.EOF. Only a plain io.EOF ends the
// archive. Any other error, io.ErrUnexpectedEOF included, is a failed read: the caller must not send
// close, or tar would see a truncated archive as a complete one.
func fillChunk(r io.Reader, buf []byte) (chunk []byte, eof bool, err error) {
	n := 0
	for n < len(buf) {
		m, rerr := r.Read(buf[n:])
		n += m
		if rerr == io.EOF {
			return buf[:n], true, nil
		}
		if rerr != nil {
			return nil, false, rerr
		}
	}
	return buf[:n], false, nil
}

// sendArchive streams the archive in one WriteProcessInput stream. It reads one message ahead, so
// the last message carries close. A guest that says tar has exited is not an error here: the caller
// reads the exit code, which tells why tar ended.
func (d *Driver) sendArchive(ctx context.Context, op string, k key, id string, first []byte, complete bool, r io.Reader) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel() // before CloseAndRecv it aborts the stream without close
	stream, err := d.process.WriteProcessInput(route(sctx, k))
	if err != nil {
		return execution.GRPCError(op, err)
	}
	// ended turns the end of a stream into the status the guest gave.
	ended := func(err error) error {
		if errors.Is(err, io.EOF) {
			_, err = stream.CloseAndRecv()
		}
		switch status.Code(err) {
		case codes.FailedPrecondition:
			return nil // the process has exited: read the exit code
		case codes.Internal:
			// tar exited, and the guest closed its stdin pipe before it recorded the exit: the write
			// or close fails with "file already closed" (see stdin.Close; interim, issue #1549). The
			// exit code says why tar ended.
			return nil
		}
		return execution.GRPCError(op, err)
	}
	var sent int64
	cur, curEOF := first, complete
	spare := make([]byte, uploadChunk)
	for {
		var next []byte
		nextEOF := false
		if !curEOF {
			var rerr error
			next, nextEOF, rerr = fillChunk(r, spare)
			if rerr != nil {
				return &execution.Error{Op: op, Err: fmt.Errorf("read archive: %w", rerr)} // cancel aborts the stream
			}
		}
		last := curEOF || (nextEOF && len(next) == 0)
		if err := stream.Send(&ateenvv1alpha.WriteProcessInputRequest{ProcessId: id, Data: cur, Close: last}); err != nil {
			return ended(err)
		}
		sent += int64(len(cur))
		if last {
			break
		}
		// cur is on the wire: Send has encoded it and no interceptor of this client keeps it, so its
		// buffer can be read into again.
		cur, spare, curEOF = next, cur[:cap(cur)], nextEOF
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return ended(err)
	}
	if resp.GetBytesWritten() != sent {
		return fail(op, execution.ErrUnavailable, "guest took %d of %d bytes", resp.GetBytesWritten(), sent)
	}
	return nil
}

// killOrphan ends a guest process that nobody reads any more. A stream that breaks does not end the
// process behind it, and a guest allows only a few processes, so an orphan would fill the cap. It
// waits for the actor to run (a suspend may have caused the break) and a signal through the router
// would resume a suspended actor, so it never signals before the gate says the actor runs. It gives
// up after GuestReadyTimeout; the StartProcess timeout is the backstop.
func (d *Driver) killOrphan(ctx context.Context, k key, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.cfg.GuestReadyTimeout)
	defer cancel()
	for {
		cctx, end, err := d.enterWait(ctx, "Upload", k, shortCall)
		if err != nil {
			return
		}
		_, err = d.process.SignalProcess(route(cctx, k), &ateenvv1alpha.SignalProcessRequest{
			ProcessId: id, Signal: ateenvv1alpha.Signal_SIGNAL_KILL,
		})
		end()
		switch status.Code(err) {
		case codes.OK, codes.NotFound, codes.FailedPrecondition:
			return // killed, or gone already
		case codes.Unavailable, codes.Internal, codes.Canceled:
			if sleep(ctx, d.cfg.PollInterval) != nil {
				return
			}
		default:
			return
		}
	}
}

// runOnce runs a command in the guest without stdin and waits for it. It returns the exit code and
// the start of its standard error. It does not re-subscribe: it is for the short commands of Upload.
func (d *Driver) runOnce(ctx context.Context, op string, k key, argv []string) (int, string, error) {
	ctx, end, err := d.enter(ctx, op, k)
	if err != nil {
		return 0, "", err
	}
	defer end()
	p, err := d.startProcess(ctx, op, k, &ateenvv1alpha.StartProcessRequest{Command: argv})
	if err != nil {
		return 0, "", err
	}
	return d.waitExit(ctx, op, k, p.GetProcessId())
}

// waitExit reads the output of a process until it exits and returns the exit code and the start of
// its standard error. The caller holds the gate.
func (d *Driver) waitExit(ctx context.Context, op string, k key, id string) (int, string, error) {
	stream, err := d.process.StreamProcessOutput(route(ctx, k), &ateenvv1alpha.StreamProcessOutputRequest{ProcessId: id, Follow: true})
	if err != nil {
		return 0, "", execution.GRPCError(op, err)
	}
	var stderr bytes.Buffer
	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = execution.ErrStreamClosed
			}
			return 0, "", execution.GRPCError(op, err)
		}
		switch o := msg.GetOutput().(type) {
		case *ateenvv1alpha.ProcessOutput_Stderr:
			if stderr.Len() < 2048 {
				stderr.Write(o.Stderr)
			}
		case *ateenvv1alpha.ProcessOutput_Exit:
			return int(o.Exit.GetExitCode()), strings.TrimSpace(stderr.String()), nil
		}
	}
}
