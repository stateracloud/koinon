// SPDX-License-Identifier: Apache-2.0

package flavors_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/acp"
)

// sandboxUID is the user OpenShell runs sandbox processes as (spike steps 1 and 8).
const sandboxUID = "10001:10001"

// sandbox is a Docker stand-in for an OpenShell sandbox: an idle container whose processes
// are started with exec, a read-only root file system, and the home (/sandbox) on a named
// volume that outlives the container, like the agent's PVC.
type sandbox struct {
	image  string
	volume string
	env    []string // KEY=VALUE, the stand-in for provider placeholders
	name   string   // current container
	starts int
}

func newSandbox(image string, env ...string) *sandbox {
	return &sandbox{image: image, volume: "koinon-contract-" + randomSuffix(), env: env}
}

// start runs a new container on the sandbox's volume.
func (s *sandbox) start(ctx context.Context) {
	s.starts++
	s.name = fmt.Sprintf("%s-%d", s.volume, s.starts)
	args := []string{
		"run", "--detach", "--name", s.name,
		"--user", sandboxUID,
		"--read-only",
		"--tmpfs", "/tmp:rw,exec,mode=1777",
		"--volume", s.volume + ":/sandbox",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		// Reaches the in-process servers; host-gateway also works on Linux runners.
		"--add-host", "host.docker.internal:host-gateway",
		"--entrypoint", "/bin/sleep",
	}
	for _, e := range s.env {
		args = append(args, "--env", e)
	}
	args = append(args, s.image, "infinity")
	docker(ctx, args...)
}

// stop removes the current container; the volume stays.
func (s *sandbox) stop(ctx context.Context) {
	if s.name != "" {
		docker(ctx, "rm", "--force", s.name)
		s.name = ""
	}
}

// remove deletes the container and the volume.
func (s *sandbox) remove(ctx context.Context) {
	s.stop(ctx)
	docker(ctx, "volume", "rm", "--force", s.volume)
}

// run executes a command in the sandbox and returns its standard output.
func (s *sandbox) run(ctx context.Context, cmd ...string) string {
	return docker(ctx, append([]string{"exec", s.name}, cmd...)...)
}

// harness starts cmd with a non-TTY exec, as koinon-agentd does through OpenShell, and
// connects an ACP client to its stdin and stdout.
func (s *sandbox) harness(ctx context.Context, workdir string, cmd []string) *harness {
	args := append([]string{"exec", "--interactive", "--workdir", workdir, s.name}, cmd...)
	c := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // test-controlled arguments
	stdin, err := c.StdinPipe()
	Expect(err).NotTo(HaveOccurred())
	stdout, err := c.StdoutPipe()
	Expect(err).NotTo(HaveOccurred())
	h := &harness{cmd: c, stdin: stdin, client: &client{}}
	c.Stderr = &h.stderr
	Expect(c.Start()).To(Succeed())
	h.conn = acp.NewConn(h.client, stdin, stdout)
	h.conn.SetLogger(newLogger())
	return h
}

// harness is one running harness process and its ACP connection.
type harness struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	conn   *acp.Conn
	client *client
	stderr lockedBuffer
}

// close ends the harness: end-of-file on stdin first, then a kill if it lingers.
func (h *harness) close() {
	_ = h.stdin.Close()
	done := make(chan struct{})
	go func() {
		_ = h.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = h.cmd.Process.Kill()
		<-done
	}
	if CurrentSpecReport().Failed() || os.Getenv("KOINON_FLAVOR_STDERR") == "1" {
		GinkgoWriter.Printf("harness stderr:\n%s\n", h.stderr.String())
	}
}

// client records what the harness sends and answers permission requests with its policy.
type client struct {
	mu          sync.Mutex
	updates     []acp.Update
	permissions []acp.PermissionRequest
	answer      []acp.PermissionOptionKind
}

func (c *client) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = append(c.updates, acp.Flatten(n))
	return nil
}

func (c *client) RequestPermission(_ context.Context, req acp.PermissionRequest) (acp.Decision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.permissions = append(c.permissions, req)
	return acp.Choose(req, c.answer...), nil
}

// answerWith sets the permission policy: the first option of the first matching kind.
func (c *client) answerWith(kinds ...acp.PermissionOptionKind) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.answer = kinds
}

// take returns and clears what was recorded.
func (c *client) take() ([]acp.Update, []acp.PermissionRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	u, p := c.updates, c.permissions
	c.updates, c.permissions = nil, nil
	return u, p
}

func docker(ctx context.Context, args ...string) string {
	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // test-controlled arguments
	c.Stdout, c.Stderr = &stdout, &stderr
	err := c.Run()
	Expect(err).NotTo(HaveOccurred(), "docker %s: %s", strings.Join(args, " "), stderr.String())
	return stdout.String()
}

func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
