// SPDX-License-Identifier: Apache-2.0

package loadtest

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// FakeMCPToken is the credential the fake MCP server expects. It is obviously fake.
const FakeMCPToken = "koinon_test_not-a-real-token-load"

// FakeMCP is a running cmd/koinon-fakemcp process.
type FakeMCP struct {
	// Port is the TCP port it listens on, on every interface.
	Port int
	cmd  *exec.Cmd
	dir  string
}

// StartFakeMCP builds cmd/koinon-fakemcp and starts it with the load tools on a free port. The
// build uses the module that contains the working directory. Stop ends it.
func StartFakeMCP(ctx context.Context) (*FakeMCP, error) {
	dir, err := os.MkdirTemp("", "koinon-load-fakemcp-")
	if err != nil {
		return nil, err
	}
	bin := filepath.Join(dir, "koinon-fakemcp")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", bin, "github.com/stateracloud/koinon/cmd/koinon-fakemcp").CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("build koinon-fakemcp: %w: %s", err, out)
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "0.0.0.0:0")
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	// Not CommandContext: the process outlives the call's context and Stop ends it.
	cmd := exec.Command(bin, "-addr", "0.0.0.0:"+strconv.Itoa(port), "-value", FakeMCPToken, "-load-tools") //nolint:gosec // a binary this function built
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	f := &FakeMCP{Port: port, cmd: cmd, dir: dir}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil {
			_ = c.Close()
			return f, nil
		}
		select {
		case <-ctx.Done():
			f.Stop()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	f.Stop()
	return nil, fmt.Errorf("koinon-fakemcp did not listen on port %d", port)
}

// Stop ends the process and removes its binary.
func (f *FakeMCP) Stop() {
	_ = f.cmd.Process.Kill()
	_, _ = f.cmd.Process.Wait()
	_ = os.RemoveAll(f.dir)
}
