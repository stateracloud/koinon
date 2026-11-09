// SPDX-License-Identifier: Apache-2.0

package openshell

// Upload speaks OpenShell's flow-controlled upload protocol, "openshell-upload-v1", to a
// sandbox's supervisor, the way `openshell sandbox upload` does, but from Go and without the CLI
// (ADR 0032, issue 169). Exec stdin has no flow control: the supervisor kills a command whose
// unread stdin passes 4 MiB with exit status 74 (supervisor ssh/input.rs MAX_PENDING_INPUT), and
// an SSH client cannot slow down, because russh grants receive credit before it hands the data to
// the application. The upload subsystem returns its own credit instead.
//
// References are to NVIDIA/OpenShell v0.1.7 (the three upload.rs files and the supervisor's
// input.rs are unchanged since v0.1.5):
//
//   - crates/openshell-core/src/upload.rs: the protocol constants and the plan line;
//   - crates/openshell-supervisor-process/src/ssh/upload.rs: the supervisor side;
//   - crates/openshell-cli/src/ssh/upload.rs: the client, which this follows (transfer, run);
//   - crates/openshell-cli/src/ssh.rs relay_ssh_proxy: ForwardTcp as the SSH transport;
//   - crates/openshell-server/src/grpc/sandbox.rs: the token and connection checks.
//
// The transport is SSH over a ForwardTcp stream. The token from CreateSshSession authorises the
// stream at the gateway; the supervisor accepts any SSH client that gets that far (ssh.rs
// auth_none and auth_publickey), so there is no SSH credential. Koinon checks the host key
// against the fingerprint CreateSshSession returns instead, as the sandbox's identity.
//
// The session token stays in this process: it is sent to the gateway only, never into the
// sandbox, and it is not logged or put into an error.

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stateracloud/koinon/internal/execution"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
)

// The protocol, from crates/openshell-core/src/upload.rs L18-31.
const (
	// UploadSubsystem is the SSH subsystem name (upload.rs SUBSYSTEM).
	UploadSubsystem = "openshell-upload-v1"
	// uploadWindow is how far the client may run ahead of the last acknowledged total
	// (WINDOW_BYTES).
	uploadWindow = 1024 * 1024
	// uploadAckInterval is the bytes between the supervisor's progress reports
	// (ACK_INTERVAL_BYTES).
	uploadAckInterval = 256 * 1024
	// uploadMaxChunk is the largest write that is always granted credit: a drained extractor
	// returns at least uploadAckInterval of credit (upload.rs L26-30).
	uploadMaxChunk = uploadWindow - uploadAckInterval
	// uploadChunk is what Koinon writes at a time. The CLI uses 64 KiB (cli ssh/upload.rs
	// CHUNK_BYTES); it must not exceed uploadMaxChunk.
	uploadChunk = 64 * 1024
	_           = uint(uploadMaxChunk - uploadChunk) // fails to compile if a chunk could outgrow the credit
	// uploadDestinationChanged is the exit status of a destination type check (not used: Koinon
	// sends no expected destination), named for the error text.
	uploadDestinationChanged = 73
	// uploadWindowExceeded is the exit status the supervisor gives a client that sent beyond its
	// window, and for every other rejected upload stream (ssh.rs reject_upload L1166-1215: invalid
	// plan, EOF before the plan, extractor start failure, not drained; upload.rs Finisher::finish).
	uploadWindowExceeded = 74
	// uploadNegotiationTimeout bounds the wait for the first (zero) total after the plan
	// (cli ssh/upload.rs NEGOTIATION_TIMEOUT).
	uploadNegotiationTimeout = 10 * time.Second
	// uploadExitWait bounds the wait for the exit status after the stream ended.
	uploadExitWait = 5 * time.Second
	// uploadStderrTail is how much of the extractor's output an error carries
	// (cli MAX_DIAGNOSTIC_BYTES is 16 KiB; errors here are shorter).
	uploadStderrTail = 4 * 1024
)

// uploadPlan is the plan line (upload.rs UploadPlan, serde field order). Koinon never sets
// expected_destination: the line is `{"destination_directory":"...","expected_destination":null}`.
type uploadPlan struct {
	DestinationDirectory string  `json:"destination_directory"`
	ExpectedDestination  *string `json:"expected_destination"`
}

// encodePlan is UploadPlan::encode: JSON and a newline, with the path checks of validate_path.
func encodePlan(dest string) ([]byte, error) {
	if dest == "" || strings.ContainsRune(dest, 0) {
		return nil, errors.New("invalid destination path")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // serde_json does not escape <, > and &
	if err := enc.Encode(uploadPlan{DestinationDirectory: dest}); err != nil {
		return nil, err
	}
	if buf.Len() > 16*1024 { // MAX_PLAN_BYTES
		return nil, errors.New("plan exceeds limit")
	}
	return buf.Bytes(), nil // Encode ends the line with "\n"
}

// Upload implements execution.Backend. See [execution.Backend.Upload].
func (b *Backend) Upload(ctx context.Context, workspace, sandbox, destDir string, archive io.Reader) (err error) {
	const op = "Upload"
	plan, err := encodePlan(destDir)
	if err != nil {
		return &execution.Error{Op: op, Kind: execution.ErrInvalid, Err: err}
	}
	// There is no timeout of the backend's own: the caller's context bounds the upload, so the
	// checkpoint timeout (Checkpoints.Timeout in agentd) is the only limit and nothing caps it
	// silently.

	sess, err := b.c.CreateSshSession(ctx, &openshellv1.CreateSshSessionRequest{
		WorkspaceScope: scope(workspace), Sandbox: sandbox,
	})
	if err != nil {
		return wrap(op, err)
	}
	// The token is revoked on every path, with a context of its own so that a cancelled upload
	// still revokes it. A failure only means the token expires on its own.
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = b.c.RevokeSshSession(rctx, &openshellv1.RevokeSshSessionRequest{Token: sess.GetToken(), AllowMissing: true})
	}()
	if sess.GetToken() == "" {
		return &execution.Error{Op: op, Kind: execution.ErrUnavailable, Err: errors.New("the gateway returned no SSH session token")}
	}
	if exp := sess.GetExpirationTime(); exp != nil && !exp.AsTime().After(time.Now()) {
		return &execution.Error{Op: op, Kind: execution.ErrPermissionDenied, Err: errors.New("the SSH session expired before it was used")}
	}

	conn, err := b.openRelay(ctx, workspace, sandbox, sess.GetToken())
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if err := runUpload(ctx, conn, sess.GetHostKeyFingerprint(), plan, archive); err != nil {
		var e *execution.Error
		if errors.As(err, &e) {
			return err
		}
		return &execution.Error{Op: op, Kind: kindOf(err), Err: err}
	}
	return nil
}

// kindOf gives the kind of a failure of the upload that is not a gRPC error.
func kindOf(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, errUploadUnsupported):
		return execution.ErrUnsupported
	}
	return nil
}

var errUploadUnsupported = errors.New("this sandbox supervisor does not support " + UploadSubsystem +
	"; OpenShell v0.1.4 or later is needed to restore a home (ADR 0032)")

// openRelay opens ForwardTcp to the sandbox's SSH target and returns it as a net.Conn.
// The first frame is TcpForwardInit with the token (relay_ssh_proxy L1971-1990).
func (b *Backend) openRelay(ctx context.Context, workspace, sandbox, token string) (*relayConn, error) {
	const op = "Upload"
	rctx, cancel := context.WithCancel(ctx)
	stream, err := b.c.ForwardTcp(rctx)
	if err != nil {
		cancel()
		return nil, wrap(op, err)
	}
	init := &openshellv1.TcpForwardFrame{Payload: &openshellv1.TcpForwardFrame_Init{Init: &openshellv1.TcpForwardInit{
		Sandbox:            sandbox,
		Workspace:          workspace,
		ServiceId:          "koinon-upload:" + sandbox,
		Target:             &openshellv1.TcpForwardInit_Ssh{Ssh: &openshellv1.SshRelayTarget{}},
		AuthorizationToken: token,
	}}}
	if err := stream.Send(init); err != nil {
		cancel()
		return nil, wrap(op, relayErr(stream, err))
	}
	c := &relayConn{stream: stream, cancel: cancel, rdone: make(chan struct{}), frames: make(chan []byte), stop: make(chan struct{})}
	go c.pump()
	return c, nil
}

// relayErr returns the stream's status when Send reports io.EOF, which only says the stream ended.
func relayErr(stream grpc.BidiStreamingClient[openshellv1.TcpForwardFrame, openshellv1.TcpForwardFrame], err error) error {
	if !errors.Is(err, io.EOF) {
		return err
	}
	if _, rerr := stream.Recv(); rerr != nil && !errors.Is(rerr, io.EOF) {
		return rerr
	}
	return execution.ErrStreamClosed
}

// relayConn is a ForwardTcp stream as a net.Conn: Write sends data frames and Read returns the
// bytes of the frames it receives. The gateway's relay is flow controlled end to end (it awaits
// each write to the supervisor; supervisor_session.rs L1074-1125), so a slow sandbox slows Write.
type relayConn struct {
	stream grpc.BidiStreamingClient[openshellv1.TcpForwardFrame, openshellv1.TcpForwardFrame]
	cancel context.CancelFunc

	wmu    sync.Mutex
	rbuf   []byte
	closed sync.Once

	// pump is the stream's only receiver. Its terminal error is kept here for Read and for Write,
	// which only waits for it.
	rmu     sync.Mutex
	rerr    error // the stream's terminal error; io.EOF for a clean end
	rdone   chan struct{}
	rdoneOn sync.Once
	frames  chan []byte
	stop    chan struct{} // closed by Close
}

func (c *relayConn) finishRead(err error) {
	c.rmu.Lock()
	c.rerr = err
	c.rmu.Unlock()
	c.rdoneOn.Do(func() { close(c.rdone) })
}

// pump is the stream's only receiver: a gRPC stream must not be received from by two goroutines,
// and Write has to learn why the stream ended even while nothing is reading (the SSH client writes
// its version line first). Data goes to Read through an unbuffered channel, so a slow reader slows
// the stream.
func (c *relayConn) pump() {
	for {
		f, err := c.stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.finishRead(io.EOF)
			} else {
				c.finishRead(wrapRelay(err))
			}
			return
		}
		select {
		case c.frames <- f.GetData():
		case <-c.stop:
			return
		}
	}
}

func (c *relayConn) Read(p []byte) (int, error) {
	for len(c.rbuf) == 0 {
		select {
		case d := <-c.frames:
			c.rbuf = d
		case <-c.rdone:
			// Frames the pump handed over are already taken; the stream is at its end.
			c.rmu.Lock()
			err := c.rerr
			c.rmu.Unlock()
			return 0, err
		}
	}
	n := copy(p, c.rbuf)
	c.rbuf = c.rbuf[n:]
	return n, nil
}

// wrapRelay turns the gateway's refusal of the token into an error that says why. The limit is
// ResourceExhausted, and an invalid, revoked or expired token is Unauthenticated (server
// sandbox.rs validate_ssh_forward_token, acquire_ssh_connection_slots).
func wrapRelay(err error) error {
	switch status.Code(err) {
	case codes.Unauthenticated:
		return &execution.Error{
			Op: "Upload", Kind: execution.ErrPermissionDenied,
			Err: fmt.Errorf("the gateway refused the SSH session (expired, revoked or for another sandbox): %s", status.Convert(err).Message()),
		}
	case codes.ResourceExhausted:
		return &execution.Error{
			Op: "Upload", Kind: execution.ErrUnavailable,
			Err: fmt.Errorf("the gateway's SSH connection limit is reached (3 per session, 20 per sandbox): %s", status.Convert(err).Message()),
		}
	}
	return wrap("Upload", err)
}

func (c *relayConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.stream.Send(&openshellv1.TcpForwardFrame{Payload: &openshellv1.TcpForwardFrame_Data{Data: append([]byte(nil), p...)}}); err != nil {
		if errors.Is(err, io.EOF) {
			// Send reports io.EOF for any ended stream; the status reaches Read, which is the
			// only receiver. Wait briefly for it instead of receiving here.
			select {
			case <-c.rdone:
			case <-time.After(time.Second):
			}
			c.rmu.Lock()
			rerr := c.rerr
			c.rmu.Unlock()
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				return 0, rerr
			}
			return 0, io.ErrClosedPipe
		}
		return 0, wrapRelay(err)
	}
	return len(p), nil
}

func (c *relayConn) Close() error {
	c.closed.Do(func() {
		c.wmu.Lock()
		_ = c.stream.CloseSend()
		c.wmu.Unlock()
		close(c.stop)
		c.cancel()
	})
	return nil
}

type relayAddr struct{}

func (relayAddr) Network() string { return "openshell-relay" }
func (relayAddr) String() string  { return "openshell-relay" }

func (c *relayConn) LocalAddr() net.Addr  { return relayAddr{} }
func (c *relayConn) RemoteAddr() net.Addr { return relayAddr{} }

// Deadlines are not used: the upload's context closes the connection.
func (c *relayConn) SetDeadline(time.Time) error      { return nil }
func (c *relayConn) SetReadDeadline(time.Time) error  { return nil }
func (c *relayConn) SetWriteDeadline(time.Time) error { return nil }

// runUpload runs the protocol over the SSH transport conn.
func runUpload(ctx context.Context, conn net.Conn, fingerprint string, plan []byte, archive io.Reader) error {
	// Cancelling ctx closes the connection, which ends every blocking call below.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	ctxErr := func(err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("%w (%w)", cerr, err)
		}
		return err
	}

	cfg := &ssh.ClientConfig{
		User:            "sandbox", // the CLI's ssh config (cli ssh.rs L2139); the supervisor ignores it
		HostKeyCallback: hostKeyCheck(fingerprint),
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, "openshell-sandbox", cfg)
	if err != nil {
		return ctxErr(fmt.Errorf("SSH to the sandbox: %w", err))
	}
	client := ssh.NewClient(c, chans, reqs)
	defer func() { _ = client.Close() }()

	ch, chReqs, err := client.OpenChannel("session", nil)
	if err != nil {
		return ctxErr(fmt.Errorf("opening an SSH channel: %w", err))
	}
	defer func() { _ = ch.Close() }()

	// Exit status requests arrive on chReqs, after the final total on stdout.
	exit := newExitWatch(chReqs)
	var stderr tailBuffer
	stderr.max = uploadStderrTail
	stderrDone := make(chan struct{})
	go func() { defer close(stderrDone); _, _ = io.Copy(&stderr, ch.Stderr()) }()

	// No PTY is requested: the supervisor refuses the subsystem on a channel that has one
	// (ssh.rs L739-742). An old supervisor refuses an unknown subsystem with channel_failure
	// (ssh.rs about L890-893), which the CLI reports as Unsupported.
	ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{UploadSubsystem}))
	if err != nil {
		return ctxErr(fmt.Errorf("requesting the upload subsystem: %w", err))
	}
	if !ok {
		return errUploadUnsupported
	}

	acks := newAckReader(ch)
	if _, err := ch.Write(plan); err != nil {
		return ctxErr(fmt.Errorf("sending the upload plan: %w", exit.explain(err, &stderr, stderrDone)))
	}
	// The first total, zero, means extraction started (upload.rs L8-9; cli transfer L288-304).
	first, err := acks.first(ctx, uploadNegotiationTimeout)
	if err != nil {
		return ctxErr(exit.explain(err, &stderr, stderrDone))
	}
	if first != 0 {
		return fmt.Errorf("upload protocol failed: upload did not start at offset zero (got %d)", first)
	}

	sent, err := sendArchive(ctx, ch, archive, acks)
	if err != nil {
		var ae *archiveError
		if errors.As(err, &ae) {
			return ae.err // the reader failed: leave without EOF so the extraction is not taken for complete
		}
		return ctxErr(exit.explain(err, &stderr, stderrDone))
	}
	// EOF marks a complete archive. The final total arrives before the exit status.
	if err := ch.CloseWrite(); err != nil {
		return ctxErr(exit.explain(fmt.Errorf("sending end of file: %w", err), &stderr, stderrDone))
	}
	acked, aerr := acks.wait(ctx)
	code, ok := exit.wait(ctx, uploadExitWait)
	<-waitOrTimeout(stderrDone, time.Second)
	switch {
	case !ok:
		if aerr != nil && ctx.Err() != nil {
			return ctxErr(aerr)
		}
		return fmt.Errorf("the sandbox's upload channel ended without an exit status after %d bytes: %s", sent, stderr.String())
	case code == uploadDestinationChanged:
		return fmt.Errorf("upload destination changed type before extraction; retry the upload: %s", stderr.String())
	case code == uploadWindowExceeded:
		// Upstream gives 74 to every rejection (reject_upload): invalid plan, EOF before the
		// plan, extractor start failure, a window overrun or an extractor not drained in 5 s.
		return fmt.Errorf("the supervisor rejected the upload stream (exit %d): %s", code, stderr.String())
	case code != 0:
		return fmt.Errorf("extraction in the sandbox exited with status %d: %s", code, stderr.String())
	case aerr != nil:
		return fmt.Errorf("upload protocol failed: reading the acknowledgments: %w", aerr)
	case acked != sent:
		return fmt.Errorf("upload protocol failed: the sandbox acknowledged %d of %d upload bytes", acked, sent)
	}
	return nil
}

// hostKeyCheck accepts only a host key whose OpenSSH SHA256 fingerprint equals the one from
// CreateSshSession ("SHA256:<base64>", the proto's documented form, which is what
// ssh.FingerprintSHA256 returns). An empty fingerprint (a legacy runtime) is refused: the
// sandbox's identity would not be checked at all.
func hostKeyCheck(want string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if want == "" {
			return errors.New("the gateway gave no host key fingerprint for the sandbox, so its identity cannot be checked")
		}
		got := ssh.FingerprintSHA256(key)
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			return fmt.Errorf("the sandbox's SSH host key %s does not match the fingerprint %s from the gateway", got, want)
		}
		return nil
	}
}

// archiveError marks a failure of the archive reader, as opposed to the transport.
type archiveError struct{ err error }

func (e *archiveError) Error() string { return e.err.Error() }
func (e *archiveError) Unwrap() error { return e.err }

// sendArchive streams the archive in chunks, never more than uploadWindow bytes beyond the last
// acknowledged total (cli ssh/upload.rs transfer L324-352), and returns the bytes sent.
//
// The archive is read by a goroutine, a chunk or two ahead (the CLI's producer queue), so that
// the archive's producer overlaps with the network and a cancelled ctx is noticed even while the
// reader blocks. On cancellation that goroutine ends when the reader returns; closing the
// reader's source is the caller's job.
func sendArchive(ctx context.Context, w io.Writer, archive io.Reader, acks *ackReader) (uint64, error) {
	type chunk struct {
		b   []byte
		err error // set on the last item: io.EOF for a complete archive
	}
	chunks := make(chan chunk, 2)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			buf := make([]byte, uploadChunk)
			n, err := io.ReadFull(archive, buf)
			if errors.Is(err, io.ErrUnexpectedEOF) {
				err = io.EOF
			}
			select {
			case chunks <- chunk{buf[:n], err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var sent uint64
	for {
		var c chunk
		select {
		case c = <-chunks:
		case <-ctx.Done():
			return sent, ctx.Err()
		}
		if n := uint64(len(c.b)); n > 0 {
			for {
				total := acks.latest()
				if total > sent {
					return sent, errors.New("upload protocol failed: upload acknowledged unsent bytes")
				}
				if sent+n-total <= uploadWindow {
					break
				}
				if err := acks.changed(ctx); err != nil {
					return sent, err
				}
			}
			if _, err := w.Write(c.b); err != nil {
				return sent, fmt.Errorf("sending the archive: %w", err)
			}
			sent += n
		}
		if c.err != nil {
			if errors.Is(c.err, io.EOF) {
				return sent, nil
			}
			return sent, &archiveError{err: fmt.Errorf("reading the archive: %w", c.err)}
		}
	}
}

// ackReader reads the supervisor's stdout, which carries only big-endian uint64 totals of the
// archive bytes written to the extractor (upload.rs L7-9).
type ackReader struct {
	mu     sync.Mutex
	total  uint64
	err    error // set when the stream ends; io.EOF is a clean end
	done   chan struct{}
	notify chan struct{} // capacity 1, signalled on every change
	got    chan struct{} // closed on the first total
}

func newAckReader(r io.Reader) *ackReader {
	a := &ackReader{done: make(chan struct{}), notify: make(chan struct{}, 1), got: make(chan struct{})}
	go a.run(r)
	return a
}

func (a *ackReader) run(r io.Reader) {
	defer close(a.done)
	var b [8]byte
	first := true
	for {
		_, err := io.ReadFull(r, b[:])
		a.mu.Lock()
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				err = errors.New("a partial acknowledgment")
			}
			a.err = err
		} else {
			t := binary.BigEndian.Uint64(b[:])
			if t < a.total {
				a.err = errors.New("upload acknowledgment moved backwards")
				err = a.err
			} else {
				a.total = t
			}
		}
		a.mu.Unlock()
		if first && err == nil {
			first = false
			close(a.got)
		}
		select {
		case a.notify <- struct{}{}:
		default:
		}
		if err != nil {
			return
		}
	}
}

func (a *ackReader) latest() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.total
}

// first waits for the first total.
func (a *ackReader) first(ctx context.Context, d time.Duration) (uint64, error) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-a.got:
		return a.latest(), nil
	case <-a.done:
		select {
		case <-a.got:
			return a.latest(), nil
		default:
		}
		return 0, a.streamEnd("before the upload started")
	case <-t.C:
		return 0, errors.New("upload negotiation timed out")
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (a *ackReader) streamEnd(when string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil && !errors.Is(a.err, io.EOF) {
		return fmt.Errorf("the upload stream failed %s: %w", when, a.err)
	}
	return fmt.Errorf("the upload stream ended %s", when)
}

// changed waits until a total arrives or the stream ends.
func (a *ackReader) changed(ctx context.Context) error {
	select {
	case <-a.notify:
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.err != nil && !errors.Is(a.err, io.EOF) {
			return fmt.Errorf("upload acknowledgments failed: %w", a.err)
		}
		return nil
	case <-a.done:
		return a.streamEnd("while the archive was being sent")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// wait waits for the end of the stdout stream and returns the final total.
func (a *ackReader) wait(ctx context.Context) (uint64, error) {
	select {
	case <-a.done:
	case <-ctx.Done():
		return a.latest(), ctx.Err()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil && !errors.Is(a.err, io.EOF) {
		return a.total, a.err
	}
	return a.total, nil
}

// exitWatch collects the channel's exit-status request.
type exitWatch struct {
	done chan struct{} // closed when the request channel ends
	mu   sync.Mutex
	code int
	have bool
}

func newExitWatch(reqs <-chan *ssh.Request) *exitWatch {
	e := &exitWatch{done: make(chan struct{})}
	go func() {
		defer close(e.done)
		for r := range reqs {
			if r.Type == "exit-status" && len(r.Payload) == 4 {
				e.mu.Lock()
				e.code, e.have = int(binary.BigEndian.Uint32(r.Payload)), true
				e.mu.Unlock()
			}
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
		}
	}()
	return e
}

func (e *exitWatch) get() (int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.code, e.have
}

// wait returns the exit status, waiting up to d for it.
func (e *exitWatch) wait(ctx context.Context, d time.Duration) (int, bool) {
	if c, ok := e.get(); ok {
		return c, true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-e.done:
	case <-t.C:
	case <-ctx.Done():
	}
	return e.get()
}

// explain adds the extractor's exit status and output to a transport failure: when the
// supervisor ends the channel (for example status 74), the write that notices only fails with EOF.
func (e *exitWatch) explain(err error, stderr *tailBuffer, stderrDone <-chan struct{}) error {
	t := time.NewTimer(time.Second)
	defer t.Stop()
	select {
	case <-e.done:
	case <-t.C:
	}
	// The extractor's output precedes the exit status on the channel, but the goroutine copying it
	// into stderr may not have run yet: read it only once the channel has been drained.
	<-waitOrTimeout(stderrDone, time.Second)
	if code, ok := e.get(); ok {
		if code == uploadWindowExceeded {
			return fmt.Errorf("%w; the supervisor rejected the upload stream (exit %d): %s", err, code, stderr.String())
		}
		return fmt.Errorf("%w; the sandbox's upload channel exited with status %d: %s", err, code, stderr.String())
	}
	if s := stderr.String(); s != "" {
		return fmt.Errorf("%w: %s", err, s)
	}
	return err
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}

func waitOrTimeout(c <-chan struct{}, d time.Duration) <-chan struct{} {
	out := make(chan struct{})
	go func() {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-c:
		case <-t.C:
		}
		close(out)
	}()
	return out
}
