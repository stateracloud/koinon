// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/execution"
	ateenvv1alpha "github.com/stateracloud/koinon/internal/gen/substrate/ateenv/v1alpha"
)

// uploadChunk is the most bytes one WriteFile message carries.
const uploadChunk = 256 << 10

// uploadDir holds the staging directory of each Upload in the guest.
const uploadDir = "/tmp"

// The staging directory and the archive in it are made so that a non-root payload can extract and
// remove what the root guest front writes (issue #1387). The payload (uid 10001 on the gVisor tiers)
// makes the directory, so it can remove all of it. Its mode lets the front, which is root without
// CAP_DAC_OVERRIDE, create the archive in it as "other", and lets no other user list it. The archive
// is readable by other users, so tar as the payload can read the file that the front owns.
const (
	stagingMode = "0733"
	archiveMode = 0o644
)

// Upload implements execution.Compute over the guest's FileSystemService. The service writes files,
// not archives, so the driver makes a staging directory in the guest, streams the tar to a file in
// it, then extracts it and removes the directory. The four fixed commands (mkdir, mkdir, tar, rm)
// run in the guest with no shell, as the payload user: the paths are arguments, never parsed.
// The guest image therefore needs mkdir, tar and rm, which the guest launcher's image contract
// lists (epic 1082, #1087). A 10 MiB archive took 0.62 s in the lab (ADR 0119, proof P-A).
//
// A read error of the tar stream cancels the write before it ends, so the guest never sees a
// complete-looking truncated archive, and is returned in the error chain.
func (d *Driver) Upload(ctx context.Context, workspace, sandbox, destDir string, archive io.Reader) error {
	const op = "Upload"
	k := key{workspace, sandbox}
	if !strings.HasPrefix(destDir, "/") {
		return fail(op, execution.ErrInvalid, "destination %q is not absolute", destDir)
	}
	if err := d.requireRunning(ctx, op, k); err != nil {
		return err
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return fmt.Errorf("substrate: %w", err)
	}
	staging := uploadDir + "/koinon-upload-" + hex.EncodeToString(rnd[:])
	tmp := staging + "/archive.tar"
	if code, msg, err := d.runOnce(ctx, op, k, []string{"mkdir", "-m", stagingMode, staging}); err != nil {
		return err
	} else if code != 0 {
		return fail(op, execution.ErrFailedPrecondition, "mkdir %s exited %d: %s", staging, code, msg)
	}
	defer func() { // best effort, and only while the actor runs: a request would resume it
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _, _ = d.runOnce(cctx, op, k, []string{"rm", "-rf", staging}) // refused when the actor is not running
	}()

	if err := d.writeFile(ctx, op, k, tmp, archive); err != nil {
		return err
	}
	if code, msg, err := d.runOnce(ctx, op, k, []string{"mkdir", "-p", destDir}); err != nil {
		return err
	} else if code != 0 {
		return fail(op, execution.ErrFailedPrecondition, "mkdir %s exited %d: %s", destDir, code, msg)
	}
	code, msg, err := d.runOnce(ctx, op, k, []string{"tar", "-x", "-f", tmp, "-C", destDir})
	if err != nil {
		return err
	}
	if code != 0 {
		return fail(op, execution.ErrInvalid, "tar exited %d: %s", code, msg)
	}
	return nil
}

// writeFile streams r into a file in the guest.
//
// A client-streaming call reports most errors late: opening it succeeds, and a guest daemon that is
// not listening yet shows only on Send (as io.EOF) or on CloseAndRecv. So the retry covers the whole
// open, send and close sequence, on Unavailable and bounded by the guest ready timeout. A retry
// needs the bytes again, and r cannot be read twice: the driver keeps the first chunk, and an
// archive that fits in it is retried to the end. A larger archive is retried while nothing beyond
// the first chunk was read; after that the error goes to the caller, who owns the stream.
func (d *Driver) writeFile(ctx context.Context, op string, k key, path string, r io.Reader) error {
	wctx, end, err := d.enter(ctx, op, k)
	if err != nil {
		return err
	}
	defer end()
	first := make([]byte, uploadChunk)
	n, rerr := io.ReadFull(r, first)
	first = first[:n]
	complete := false
	switch {
	case rerr == nil:
	case errors.Is(rerr, io.EOF), errors.Is(rerr, io.ErrUnexpectedEOF):
		complete = true
	default:
		return &execution.Error{Op: op, Err: fmt.Errorf("read archive: %w", rerr)}
	}
	deadline := time.Now().Add(d.cfg.GuestReadyTimeout)
	delay := d.cfg.PollInterval
	for {
		retry, err := d.writeAttempt(wctx, op, k, path, first, complete, r)
		if err == nil {
			return nil
		}
		if !retry || !errors.Is(err, execution.ErrUnavailable) || time.Now().After(deadline) {
			return err
		}
		if err := sleep(ctx, delay); err != nil {
			return execution.GRPCError(op, err)
		}
		delay = min(delay*2, 8*d.cfg.PollInterval)
	}
}

// writeAttempt is one open, send and close of the file. retry says whether the bytes it consumed
// from r can be sent again.
func (d *Driver) writeAttempt(ctx context.Context, op string, k key, path string, first []byte, complete bool, r io.Reader) (retry bool, _ error) {
	actx, cancel := context.WithCancel(ctx)
	defer cancel() // ends the call; before CloseAndRecv it aborts without end-of-file
	stream, err := d.files.WriteFile(route(actx, k))
	if err != nil {
		return true, execution.GRPCError(op, err)
	}
	sent := int64(len(first))
	msg := &ateenvv1alpha.WriteFileRequest{Path: path, Mode: archiveMode, Chunk: first}
	if err := stream.Send(msg); err != nil {
		if errors.Is(err, io.EOF) { // the guest ended the stream: its status says why
			_, err = stream.CloseAndRecv()
		}
		return true, execution.GRPCError(op, err)
	}
	retry = complete
	buf := make([]byte, uploadChunk)
	for !complete {
		n, rerr := r.Read(buf)
		if n > 0 {
			if err := stream.Send(&ateenvv1alpha.WriteFileRequest{Chunk: buf[:n]}); err != nil {
				if errors.Is(err, io.EOF) {
					_, err = stream.CloseAndRecv()
				}
				return false, execution.GRPCError(op, err)
			}
			sent += int64(n)
		}
		if rerr == nil {
			continue
		}
		if rerr == io.EOF {
			break
		}
		return false, &execution.Error{Op: op, Err: fmt.Errorf("read archive: %w", rerr)} // cancel aborts the write
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return retry, execution.GRPCError(op, err)
	}
	if resp.GetBytesWritten() != sent {
		return false, fail(op, execution.ErrUnavailable, "guest wrote %d of %d bytes", resp.GetBytesWritten(), sent)
	}
	return false, nil
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
	stream, err := d.process.StreamProcessOutput(route(ctx, k), &ateenvv1alpha.StreamProcessOutputRequest{ProcessId: p.GetProcessId(), Follow: true})
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
