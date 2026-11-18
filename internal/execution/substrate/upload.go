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

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stateracloud/koinon/internal/execution"
	ateenvv1alpha "github.com/stateracloud/koinon/internal/gen/substrate/ateenv/v1alpha"
)

// uploadChunk is the most bytes one WriteFile message carries.
const uploadChunk = 256 << 10

// uploadDir holds the archive in the guest while it is extracted.
const uploadDir = "/tmp"

// Upload implements execution.Compute over the guest's FileSystemService. The service writes files,
// not archives, so the driver streams the tar to a temporary file in the guest, then runs three
// fixed commands (mkdir, tar, rm) in the guest with no shell: the paths are arguments, never parsed.
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
	tmp := uploadDir + "/koinon-upload-" + hex.EncodeToString(rnd[:]) + ".tar"
	defer func() { // best effort, and only while the actor runs: a request would resume it
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _, _ = d.runOnce(cctx, op, k, []string{"rm", "-f", tmp}) // refused when the actor is not running
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
func (d *Driver) writeFile(ctx context.Context, op string, k key, path string, r io.Reader) error {
	wctx, end, err := d.enter(ctx, op, k)
	if err != nil {
		return err
	}
	defer end()
	cancel := end // aborts without end-of-file
	var stream ateenvv1alpha.FileSystemService_WriteFileClient
	deadline := time.Now().Add(d.cfg.GuestReadyTimeout)
	delay := d.cfg.PollInterval
	for {
		s, err := d.files.WriteFile(route(wctx, k))
		if err == nil {
			stream = s
			break
		}
		if status.Code(err) != codes.Unavailable || time.Now().After(deadline) {
			return execution.GRPCError(op, err)
		}
		if err := sleep(ctx, delay); err != nil {
			return execution.GRPCError(op, err)
		}
	}
	buf := make([]byte, uploadChunk)
	var sent int64
	first := true
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			msg := &ateenvv1alpha.WriteFileRequest{Chunk: buf[:n]}
			if first {
				msg.Path, msg.Mode, first = path, 0o600, false
			}
			if err := stream.Send(msg); err != nil {
				if errors.Is(err, io.EOF) { // the guest ended the stream: its status says why
					_, err = stream.CloseAndRecv()
				}
				return execution.GRPCError(op, err)
			}
			sent += int64(n)
		}
		if rerr == nil {
			continue
		}
		if rerr == io.EOF {
			break
		}
		cancel() // abort without end-of-file
		return &execution.Error{Op: op, Err: fmt.Errorf("read archive: %w", rerr)}
	}
	if first { // an empty archive still makes a file, so that tar reports it
		if err := stream.Send(&ateenvv1alpha.WriteFileRequest{Path: path, Mode: 0o600}); err != nil && !errors.Is(err, io.EOF) {
			return execution.GRPCError(op, err)
		}
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return execution.GRPCError(op, err)
	}
	if resp.GetBytesWritten() != sent {
		return fail(op, execution.ErrUnavailable, "guest wrote %d of %d bytes", resp.GetBytesWritten(), sent)
	}
	return nil
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
