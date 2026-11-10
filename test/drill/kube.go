// SPDX-License-Identifier: Apache-2.0

package drill

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Kube runs kubectl and helm against one explicit kubeconfig and context. Nothing here reads the
// ambient context (AGENTS.md: explicit cluster, always); New refuses an empty kubeconfig.
type Kube struct {
	Kubeconfig string
	Context    string
	// BinDir holds kubectl and helm wrappers pinned to the context, first on PATH for the guide's blocks.
	BinDir string
}

// NewKube checks the kubeconfig and writes the wrappers into dir.
func NewKube(kubeconfig, kubeContext, dir string) (*Kube, error) {
	if kubeconfig == "" || kubeconfig == "/dev/null" {
		return nil, fmt.Errorf("KUBECONFIG must name the cluster's kubeconfig file explicitly")
	}
	if kubeContext == "" {
		return nil, fmt.Errorf("the kube context is empty")
	}
	k := &Kube{Kubeconfig: kubeconfig, Context: kubeContext, BinDir: dir}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for tool, flag := range map[string]string{"kubectl": "--context", "helm": "--kube-context"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			return nil, err
		}
		body := fmt.Sprintf("#!/usr/bin/env bash\nexport KUBECONFIG=%q\nexec %q %s %q \"$@\"\n", kubeconfig, real, flag, kubeContext)
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(body), 0o755); err != nil { //nolint:gosec // a wrapper script must be executable
			return nil, err
		}
	}
	return k, nil
}

func (k *Kube) cmd(ctx context.Context, tool string, args ...string) *exec.Cmd {
	flag := "--context"
	if tool == "helm" {
		flag = "--kube-context"
	}
	c := exec.CommandContext(ctx, tool, append([]string{flag, k.Context}, args...)...)
	c.Env = append(os.Environ(), "KUBECONFIG="+k.Kubeconfig)
	return c
}

func run(c *exec.Cmd) (string, error) {
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return out.String(), fmt.Errorf("%s: %w: %s", strings.Join(c.Args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Kubectl runs kubectl and returns stdout.
func (k *Kube) Kubectl(ctx context.Context, args ...string) (string, error) {
	return run(k.cmd(ctx, "kubectl", args...))
}

// KubectlStdin runs kubectl with stdin.
func (k *Kube) KubectlStdin(ctx context.Context, stdin string, args ...string) (string, error) {
	c := k.cmd(ctx, "kubectl", args...)
	c.Stdin = strings.NewReader(stdin)
	return run(c)
}

// Helm runs helm and returns stdout.
func (k *Kube) Helm(ctx context.Context, args ...string) (string, error) {
	return run(k.cmd(ctx, "helm", args...))
}

// Exec runs a command in a pod or deployment (target is "deploy/x" or "pod/x").
func (k *Kube) Exec(ctx context.Context, ns, target string, command ...string) (string, error) {
	return k.Kubectl(ctx, append([]string{"-n", ns, "exec", target, "--"}, command...)...)
}

// Primary returns the CNPG primary pod of an install.
func (k *Kube) Primary(ctx context.Context, ns, release string) (string, error) {
	out, err := k.Kubectl(ctx, "-n", ns, "get", "pod", "-l", "cnpg.io/cluster="+release+"-pg,cnpg.io/instanceRole=primary", "-o", "name")
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(out)
	if name == "" || strings.Contains(name, "\n") {
		return "", fmt.Errorf("want one CNPG primary in %s for %s, got %q", ns, release, name)
	}
	return name, nil
}

// SQL runs one read or write statement as the install's database owner and returns its rows
// (psql -At, fields separated by |). The drill reads state with it; it seeds nothing with it.
func (k *Kube) SQL(ctx context.Context, ns, release, query string) (string, error) {
	pod, err := k.Primary(ctx, ns, release)
	if err != nil {
		return "", err
	}
	out, err := k.Kubectl(ctx, "-n", ns, "exec", pod, "-c", "postgres", "--", "psql", "--dbname=koinon", "-v", "ON_ERROR_STOP=1", "-Atq", "-c", query)
	return strings.TrimSpace(out), err
}

var forwardRE = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+) ->`)

// PortForward forwards a random local port to target's port and returns "127.0.0.1:<port>" and a
// stop function. The forward ends when ctx ends or stop is called.
func (k *Kube) PortForward(ctx context.Context, ns, target string, port int) (string, func(), error) {
	pctx, cancel := context.WithCancel(ctx)
	c := k.cmd(pctx, "kubectl", "-n", ns, "port-forward", "--address", "127.0.0.1", target, fmt.Sprintf(":%d", port))
	stdout, err := c.StdoutPipe()
	if err != nil {
		cancel()
		return "", nil, err
	}
	var errb bytes.Buffer
	c.Stderr = &errb
	if err := c.Start(); err != nil {
		cancel()
		return "", nil, err
	}
	stop := func() { cancel(); _ = c.Wait() }
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if m := forwardRE.FindStringSubmatch(sc.Text()); m != nil {
				found <- "127.0.0.1:" + m[1]
				break
			}
		}
		for sc.Scan() { // keep draining so the process never blocks on a full pipe
		}
	}()
	select {
	case addr := <-found:
		return addr, stop, nil
	case <-time.After(30 * time.Second):
		stop()
		return "", nil, fmt.Errorf("port-forward to %s/%s did not start: %s", ns, target, strings.TrimSpace(errb.String()))
	case <-ctx.Done():
		stop()
		return "", nil, ctx.Err()
	}
}

// Label adds the run label to objects: `kubectl label <kind> <names...> koinon.dev/drill-run=<id>`.
func (k *Kube) Label(ctx context.Context, runID, ns string, args ...string) error {
	a := []string{"label", "--overwrite"}
	if ns != "" {
		a = append(a, "-n", ns)
	}
	a = append(a, args...)
	a = append(a, RunLabel+"="+runID)
	_, err := k.Kubectl(ctx, a...)
	return err
}

// GuideBlock runs one block of the backup and restore guide, as written, for the install in ns with
// the given release, in workdir (where the dump and the key backup live). It returns the combined output.
func (k *Kube) GuideBlock(ctx context.Context, repoRoot, workdir, name, ns, release string) (string, error) {
	show := exec.CommandContext(ctx, filepath.Join(repoRoot, "hack", "docs-e1.sh"), "show", name)
	show.Env = append(os.Environ(), "E1_FILE="+filepath.Join(repoRoot, "test", "e2e", "commands", "backup-restore.e1"))
	text, err := run(show)
	if err != nil {
		return "", err
	}
	script, err := Substitute(text, ns, release)
	if err != nil {
		return "", fmt.Errorf("guide block %s: %w", name, err)
	}
	c := exec.CommandContext(ctx, "bash", "-euo", "pipefail", "-c", script)
	c.Dir = workdir
	c.Env = append(os.Environ(), "KUBECONFIG="+k.Kubeconfig, "PATH="+k.BinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if err := c.Run(); err != nil {
		return out.String(), fmt.Errorf("guide block %s: %w\n%s", name, err, out.String())
	}
	return out.String(), nil
}
