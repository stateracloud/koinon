// SPDX-License-Identifier: Apache-2.0

package harnesses_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/acp"
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
		// Tells koinon-agentd's sweep that PID 1 (sleep) is the main process, as the OpenShell
		// supervisor's first child is in a real sandbox (internal/agentd/sweep.sh).
		"--env", "KOINON_SANDBOX_STANDIN=1",
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

// try executes a command in the sandbox and returns its output and exit code; unlike run it
// does not fail the spec when the command fails.
func (s *sandbox) try(ctx context.Context, cmd ...string) (stdout, stderr string, code int) {
	var o, e bytes.Buffer
	c := exec.CommandContext(ctx, "docker", append([]string{"exec", s.name}, cmd...)...)
	c.Stdout, c.Stderr = &o, &e
	err := c.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		Expect(err).NotTo(HaveOccurred())
	}
	return o.String(), e.String(), code
}

// write puts content at path in the sandbox, creating the parent directories.
func (s *sandbox) write(ctx context.Context, path, content string) {
	s.run(ctx, "sh", "-c", `mkdir -p "$(dirname "$1")" && printf '%s' "$2" > "$1"`, "sh", path, content)
}

// harness starts cmd with a non-TTY exec, as koinon-agentd does through OpenShell, and
// connects an ACP client to its stdin and stdout.
func (s *sandbox) harness(ctx context.Context, workdir string, cmd []string) *harness {
	args := append([]string{"exec", "--interactive", "--workdir", workdir, s.name}, cmd...)
	c := exec.CommandContext(ctx, "docker", args...)
	stdin, err := c.StdinPipe()
	Expect(err).NotTo(HaveOccurred())
	stdout, err := c.StdoutPipe()
	Expect(err).NotTo(HaveOccurred())
	h := &harness{cmd: c, stdin: stdin, client: &client{}}
	c.Stderr = &h.stderr
	Expect(c.Start()).To(Succeed())
	h.conn = acp.NewConn(context.Background(), h.client, stdin, stdout)
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
	if CurrentSpecReport().Failed() || os.Getenv("KOINON_HARNESS_STDERR") == "1" {
		GinkgoWriter.Printf("harness stderr:\n%s\n", h.stderr.String())
	}
}

// client records what the harness sends and answers permission requests with its policy.
type client struct {
	mu          sync.Mutex
	updates     []acp.Update
	permissions []acp.PermissionRequest
	answer      []acp.PermissionOptionKind
	// commands is the latest available_commands_update; take does not clear it.
	commands []string
}

func (c *client) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := acp.Flatten(n)
	if u.Kind == "available_commands_update" {
		c.commands = u.Commands
	}
	c.updates = append(c.updates, u)
	return nil
}

// availableCommands returns the commands the harness announced last.
func (c *client) availableCommands() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.commands...)
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
	c := exec.CommandContext(ctx, "docker", args...)
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
