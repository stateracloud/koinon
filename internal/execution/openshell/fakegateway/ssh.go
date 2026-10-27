// SPDX-License-Identifier: Apache-2.0

package fakegateway

// The SSH side of the fake gateway: CreateSshSession, RevokeSshSession and ForwardTcp, with an
// in-process SSH server behind the relay that implements the supervisor's "openshell-upload-v1"
// subsystem as OpenShell v0.1.5 does (crates/openshell-core/src/upload.rs and
// crates/openshell-supervisor-process/src/ssh/upload.rs):
//
//   - the client sends one JSON plan line, then the raw archive, then EOF;
//   - stdout carries only big-endian uint64 totals of archive bytes the extractor has read:
//     zero when extraction starts, one every 256 KiB, and the final total on success;
//   - the client may be at most 1 MiB past the last total (Upload.admits); a client that sends
//     more is killed with exit status 74;
//   - the exit status is the result; stderr carries the extractor's output.
//
// Plain exec has the supervisor's pending-stdin cap of 4 MiB (ssh/input.rs MAX_PENDING_INPUT),
// which kills a command whose unread stdin passes it with exit status 74 (see exec.go).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	datamodelv1 "github.com/stateracloud/koinon/internal/gen/openshell/datamodel/v1"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
)

// Protocol constants, as in upstream's upload.rs.
const (
	UploadSubsystem  = "openshell-upload-v1"
	UploadWindow     = 1024 * 1024
	UploadAckEvery   = 256 * 1024
	maxPlanBytes     = 16 * 1024
	maxTokenForwards = 3 // MAX_CONNECTIONS_PER_TOKEN
	// MaxPendingStdin is the supervisor's cap on exec stdin the command has not read yet.
	MaxPendingStdin = 4 * 1024 * 1024
)

// UploadHandler stands in for `cat | tar xf - -C dest`. It reads the archive until EOF or until
// it has what it needs (the fake reads the rest, as cat would) and returns the exit code.
type UploadHandler func(ctx context.Context, dest string, archive io.Reader, stderr io.Writer) int

// UploadStats are what the fake saw of uploads.
type UploadStats struct {
	// Uploads is how many upload subsystem sessions started extraction.
	Uploads int
	// Bytes is the archive bytes received.
	Bytes uint64
	// MaxOutstanding is the most archive bytes the fake held that the extractor had not read.
	MaxOutstanding uint64
	// Violations counts clients that sent beyond the window (each was killed with 74).
	Violations int
}

type sshSession struct {
	sandboxID string
	workspace string
	sandbox   string
	expired   bool
	revoked   bool
}

type sshState struct {
	mu       sync.Mutex
	signer   ssh.Signer
	sessions map[string]*sshSession
	forwards map[string]int // by token
	handler  UploadHandler
	noUpload bool
	spoof    string
	expire   bool
	omitFP   bool
	stats    UploadStats
	seq      int
}

func newSSHState() *sshState {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		panic(err)
	}
	return &sshState{signer: signer, sessions: map[string]*sshSession{}, forwards: map[string]int{}}
}

// HandleUpload sets the extractor the upload subsystem runs. Without one, uploads are read and
// discarded.
func (g *Gateway) HandleUpload(h UploadHandler) {
	g.ssh.mu.Lock()
	defer g.ssh.mu.Unlock()
	g.ssh.handler = h
}

// DisableUploadSubsystem makes the supervisor refuse the subsystem, as one older than OpenShell
// v0.1.4 does.
func (g *Gateway) DisableUploadSubsystem() {
	g.ssh.mu.Lock()
	defer g.ssh.mu.Unlock()
	g.ssh.noUpload = true
}

// HostKeyFingerprint is the SSH host key's OpenSSH SHA256 fingerprint.
func (g *Gateway) HostKeyFingerprint() string {
	return ssh.FingerprintSHA256(g.ssh.signer.PublicKey())
}

// SpoofHostKeyFingerprint makes CreateSshSession report fp instead of the SSH server's real
// fingerprint, as a man in the middle or a stale record would look to a client.
func (g *Gateway) SpoofHostKeyFingerprint(fp string) {
	g.ssh.mu.Lock()
	defer g.ssh.mu.Unlock()
	g.ssh.spoof = fp
}

// OmitHostKeyFingerprint makes CreateSshSession report no fingerprint, as a legacy runtime does.
func (g *Gateway) OmitHostKeyFingerprint() {
	g.ssh.mu.Lock()
	defer g.ssh.mu.Unlock()
	g.ssh.omitFP = true
}

// ExpireSSHSessions makes the gateway treat every session token, including those it issues
// later, as expired.
func (g *Gateway) ExpireSSHSessions() {
	g.ssh.mu.Lock()
	defer g.ssh.mu.Unlock()
	g.ssh.expire = true
	for _, s := range g.ssh.sessions {
		s.expired = true
	}
}

// ActiveSSHSessions is how many session tokens have not been revoked.
func (g *Gateway) ActiveSSHSessions() int {
	g.ssh.mu.Lock()
	defer g.ssh.mu.Unlock()
	n := 0
	for _, s := range g.ssh.sessions {
		if !s.revoked {
			n++
		}
	}
	return n
}

// SSHTokens returns the session tokens issued so far, for tests that check a token never
// appears elsewhere.
func (g *Gateway) SSHTokens() []string {
	g.ssh.mu.Lock()
	defer g.ssh.mu.Unlock()
	var out []string
	for t := range g.ssh.sessions {
		out = append(out, t)
	}
	return out
}

// UploadStats returns what the fake saw of uploads.
func (g *Gateway) UploadStats() UploadStats {
	g.ssh.mu.Lock()
	defer g.ssh.mu.Unlock()
	return g.ssh.stats
}

// CreateSshSession implements openshellv1.OpenShellServer.
func (g *Gateway) CreateSshSession(_ context.Context, req *openshellv1.CreateSshSessionRequest) (*openshellv1.CreateSshSessionResponse, error) {
	g.mu.Lock()
	if err := g.enter("CreateSshSession"); err != nil {
		g.mu.Unlock()
		return nil, err
	}
	sb, k, err := g.sandbox(req.GetWorkspaceScope(), req.GetSandbox())
	id := sb.GetMetadata().GetId()
	g.mu.Unlock()
	if err != nil {
		return nil, err
	}
	g.ssh.mu.Lock()
	defer g.ssh.mu.Unlock()
	g.ssh.seq++
	token := fmt.Sprintf("koinon_test_not-a-real-token-%d", g.ssh.seq)
	g.ssh.sessions[token] = &sshSession{sandboxID: id, workspace: k.workspace, sandbox: k.name}
	fp := g.HostKeyFingerprint()
	if g.ssh.spoof != "" {
		fp = g.ssh.spoof
	}
	if g.ssh.omitFP {
		fp = ""
	}
	return &openshellv1.CreateSshSessionResponse{
		SandboxId: id, Token: token, GatewayHost: "fakegateway", GatewayPort: 8080, GatewayScheme: "http",
		HostKeyFingerprint: fp, ExpirationTime: timestamppb.New(time.Now().Add(24 * time.Hour)),
	}, nil
}

// RevokeSshSession implements openshellv1.OpenShellServer.
func (g *Gateway) RevokeSshSession(_ context.Context, req *openshellv1.RevokeSshSessionRequest) (*openshellv1.RevokeSshSessionResponse, error) {
	g.mu.Lock()
	err := g.enter("RevokeSshSession")
	g.mu.Unlock()
	if err != nil {
		return nil, err
	}
	g.ssh.mu.Lock()
	defer g.ssh.mu.Unlock()
	s, ok := g.ssh.sessions[req.GetToken()]
	if !ok {
		if req.GetAllowMissing() {
			return &openshellv1.RevokeSshSessionResponse{Outcome: openshellv1.DeletionOutcome_DELETION_OUTCOME_ALREADY_ABSENT}, nil
		}
		return nil, status.Error(codes.NotFound, "SSH session not found")
	}
	s.revoked = true
	return &openshellv1.RevokeSshSessionResponse{Outcome: openshellv1.DeletionOutcome_DELETION_OUTCOME_COMPLETED}, nil
}

// ForwardTcp implements openshellv1.OpenShellServer for SSH targets: it checks the token as the
// gateway does (sandbox.rs validate_ssh_forward_token), then serves SSH on the stream.
func (g *Gateway) ForwardTcp(stream grpc.BidiStreamingServer[openshellv1.TcpForwardFrame, openshellv1.TcpForwardFrame]) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to read the init frame: %v", err)
	}
	init := first.GetInit()
	if init == nil {
		return status.Error(codes.InvalidArgument, "the first frame must be init")
	}
	g.mu.Lock()
	err = g.enter("ForwardTcp")
	var sb *openshellv1.Sandbox
	if err == nil {
		sb, _, err = g.sandbox(&datamodelv1.WorkspaceSelector{Selection: &datamodelv1.WorkspaceSelector_Workspace{Workspace: init.GetWorkspace()}}, init.GetSandbox())
	}
	id := sb.GetMetadata().GetId()
	g.mu.Unlock()
	if err != nil {
		return err
	}
	if init.GetSsh() == nil {
		return status.Error(codes.Unimplemented, "fakegateway forwards SSH targets only")
	}
	release, err := g.ssh.admit(init.GetAuthorizationToken(), id)
	if err != nil {
		return err
	}
	defer release()

	client, server := net.Pipe()
	served := make(chan struct{})
	go func() { defer close(served); g.serveSSH(stream.Context(), server) }()
	go func() {
		defer func() { _ = client.Close() }()
		for {
			f, err := stream.Recv()
			if err != nil {
				return
			}
			if d := f.GetData(); len(d) > 0 {
				if _, err := client.Write(d); err != nil {
					return
				}
			}
		}
	}()
	buf := make([]byte, 32*1024)
	for {
		n, err := client.Read(buf)
		if n > 0 {
			if serr := stream.Send(&openshellv1.TcpForwardFrame{Payload: &openshellv1.TcpForwardFrame_Data{Data: bytes.Clone(buf[:n])}}); serr != nil {
				_ = client.Close()
				<-served
				return serr
			}
		}
		if err != nil {
			break
		}
	}
	<-served
	return nil
}

func (s *sshState) admit(token, sandboxID string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token == "" {
		return nil, status.Error(codes.Unauthenticated, "authorization_token is required for SSH targets")
	}
	sess, ok := s.sessions[token]
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "SSH session token not found")
	}
	if sess.revoked || sess.sandboxID != sandboxID {
		return nil, status.Error(codes.Unauthenticated, "SSH session token is not valid")
	}
	if sess.expired || s.expire {
		return nil, status.Error(codes.Unauthenticated, "SSH session token expired")
	}
	if s.forwards[token] >= maxTokenForwards {
		return nil, status.Error(codes.ResourceExhausted, "SSH session connection limit reached")
	}
	s.forwards[token]++
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.forwards[token]--
	}, nil
}

// serveSSH runs the supervisor's SSH server on conn: no authentication (ssh.rs auth_none) and
// session channels with the upload subsystem.
func (g *Gateway) serveSSH(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(g.ssh.signer)
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sc.Close() }()
	go ssh.DiscardRequests(reqs)
	stop := context.AfterFunc(ctx, func() { _ = sc.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "only session channels")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); g.session(ctx, ch, creqs) }()
	}
}

func (g *Gateway) session(ctx context.Context, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer func() { _ = ch.Close() }()
	pty := false
	for r := range reqs {
		if r.Type == "pty-req" {
			// The supervisor remembers the PTY and refuses the upload subsystem on such a channel
			// (ssh.rs L739-742).
			pty = true
			if r.WantReply {
				_ = r.Reply(true, nil)
			}
			continue
		}
		if r.Type != "subsystem" {
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
			continue
		}
		var p struct{ Name string }
		g.ssh.mu.Lock()
		refuse := pty || ssh.Unmarshal(r.Payload, &p) != nil || p.Name != UploadSubsystem || g.ssh.noUpload
		g.ssh.mu.Unlock()
		if refuse {
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
			continue
		}
		if r.WantReply {
			_ = r.Reply(true, nil)
		}
		g.upload(ctx, ch)
		return
	}
}

type uploadPlan struct {
	DestinationDirectory string  `json:"destination_directory"`
	ExpectedDestination  *string `json:"expected_destination"`
}

func exitStatus(ch ssh.Channel, code int) {
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)})) //nolint:gosec // small
}

func ack(total uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, total)
	return b
}

// upload serves one upload subsystem channel (ssh/upload.rs and ssh.rs data handling).
func (g *Gateway) upload(ctx context.Context, ch ssh.Channel) {
	fail := func(code int, msg string) {
		// A startup failure carries a message; a rejection later must not queue output
		// (ssh.rs reject_upload).
		if msg != "" {
			_, _ = io.WriteString(ch.Stderr(), msg+"\n")
		}
		exitStatus(ch, code)
	}
	// The plan line: read up to the first newline, keep what follows it as archive bytes.
	var line []byte
	var rest []byte
	buf := make([]byte, 32*1024)
	for rest == nil {
		n, err := ch.Read(buf)
		if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
			line = append(line, buf[:i]...)
			rest = bytes.Clone(buf[i+1 : n])
			if rest == nil {
				rest = []byte{}
			}
		} else {
			line = append(line, buf[:n]...)
		}
		if len(line) > maxPlanBytes {
			fail(74, "openshell: invalid upload plan: plan exceeds limit")
			return
		}
		if err != nil && rest == nil {
			fail(74, "")
			return
		}
	}
	var plan uploadPlan
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&plan); err != nil || plan.DestinationDirectory == "" {
		fail(74, fmt.Sprintf("openshell: invalid upload plan: %v", err))
		return
	}

	q := newInputQueue()
	if len(rest) > 0 {
		q.push(rest)
	}
	g.ssh.mu.Lock()
	h := g.ssh.handler
	g.ssh.stats.Uploads++
	g.ssh.mu.Unlock()

	// Extraction starts: the first total, zero (ssh/upload.rs send_acks).
	if _, err := ch.Write(ack(0)); err != nil {
		return
	}
	var ackMu sync.Mutex // orders acks, so a late periodic ack cannot follow the final one
	finished := make(chan int, 1)
	// An extractor that fails while the client still sends ends the channel at once, with its exit
	// status and no final total, as the supervisor does: it stops reading and closes stdout.
	var killMu sync.Mutex
	killed := false
	var stopOnce sync.Once
	stopAcks := make(chan struct{})
	stop := func() { stopOnce.Do(func() { close(stopAcks) }) }
	go func() {
		var code int
		if h == nil {
			_, _ = io.Copy(io.Discard, q)
		} else {
			code = h(ctx, plan.DestinationDirectory, q, ch.Stderr())
		}
		if code != 0 {
			killMu.Lock()
			killed = true
			killMu.Unlock()
			q.fail(io.ErrClosedPipe)
			stop()
			ackMu.Lock()
			exitStatus(ch, code)
			_ = ch.Close()
			ackMu.Unlock()
			finished <- code
			return
		}
		// cat reads the rest of the archive after tar stops.
		_, _ = io.Copy(io.Discard, q)
		finished <- code
	}()
	wasKilled := func() bool { killMu.Lock(); defer killMu.Unlock(); return killed }
	acksDone := make(chan struct{})
	go func() {
		defer close(acksDone)
		var acked uint64
		for {
			select {
			case <-q.notify:
			case <-stopAcks:
				return
			}
			if c := q.consumedBytes(); c-acked >= UploadAckEvery {
				ackMu.Lock()
				_, err := ch.Write(ack(c))
				ackMu.Unlock()
				if err != nil {
					return
				}
				acked = c
			}
		}
	}()

	// Receive the archive, enforcing the window (Upload::admits).
	for {
		n, err := ch.Read(buf)
		if n > 0 {
			outstanding := q.received() + uint64(n) - q.consumedBytes()
			g.ssh.mu.Lock()
			g.ssh.stats.Bytes += uint64(n)
			g.ssh.stats.MaxOutstanding = max(g.ssh.stats.MaxOutstanding, outstanding)
			g.ssh.mu.Unlock()
			if outstanding > UploadWindow {
				g.ssh.mu.Lock()
				g.ssh.stats.Violations++
				g.ssh.mu.Unlock()
				stop()
				<-acksDone
				q.fail(errors.New("window exceeded"))
				fail(74, "")
				return
			}
			q.push(bytes.Clone(buf[:n]))
		}
		if err != nil {
			break
		}
	}
	if wasKilled() {
		<-finished
		<-acksDone
		return
	}
	q.closeInput()
	code := <-finished
	stop()
	<-acksDone
	if code == 0 {
		ackMu.Lock()
		_, _ = ch.Write(ack(q.consumedBytes()))
		ackMu.Unlock()
	}
	exitStatus(ch, code)
}

// inputQueue is the supervisor's pending stdin: bytes received and not yet read by the process.
type inputQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	chunks  [][]byte
	pending int
	recv    uint64
	used    uint64
	closed  bool
	err     error
	notify  chan struct{} // capacity 1, signalled when bytes are read
}

func newInputQueue() *inputQueue {
	q := &inputQueue{notify: make(chan struct{}, 1)}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *inputQueue) push(b []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.chunks = append(q.chunks, b)
	q.pending += len(b)
	q.recv += uint64(len(b))
	q.cond.Broadcast()
}

// closeInput ends the input: reads return io.EOF once the queue is empty.
func (q *inputQueue) closeInput() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.cond.Broadcast()
}

// fail makes reads return err at once.
func (q *inputQueue) fail(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.err = err
	q.cond.Broadcast()
}

func (q *inputQueue) pendingBytes() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pending
}

func (q *inputQueue) received() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.recv
}

func (q *inputQueue) consumedBytes() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.used
}

func (q *inputQueue) Read(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.chunks) == 0 && !q.closed && q.err == nil {
		q.cond.Wait()
	}
	if q.err != nil {
		return 0, q.err
	}
	if len(q.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, q.chunks[0])
	q.chunks[0] = q.chunks[0][n:]
	if len(q.chunks[0]) == 0 {
		q.chunks = q.chunks[1:]
	}
	q.pending -= n
	q.used += uint64(n) //nolint:gosec // n is a count of bytes read
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return n, nil
}
