// SPDX-License-Identifier: Apache-2.0

// Command drillwebrun runs one web/e2e-drill spec in the pinned Playwright image (#1039), so the drill job
// installs no browser and no OS package from a live mirror (hack/ci-playwright-image-test.sh, #569). The
// drill host (the CI runner) needs kind and the cluster port-forwards, so the job itself cannot run in the
// image; only the spec does. It replaces hack/drill-web-run.sh (ADR 0118, #1072).
//
// Usage, from inside the repository (test/drill does this when KOINON_DRILL_PLAYWRIGHT_IMAGE is set):
//
//	KOINON_DRILL_PLAYWRIGHT_IMAGE=mcr.microsoft.com/playwright:v<ver>-noble@sha256:... go tool drillwebrun <spec>
//
// web/node_modules comes from the host's `pnpm --dir web install --frozen-lockfile`; the image brings
// Chromium and its libraries. The container runs as the host user (--user uid:gid), with --network=host: the
// spec reaches the port-forwards on the host's 127.0.0.1. Variables pass by name, never by value, so the
// passwords stay out of the process list.
//
// Output directory (#1072): Playwright removes its outputDir at start. A bind-mount point cannot be removed,
// so mounting PLAYWRIGHT_OUTPUT_DIR itself failed with EACCES on rmdir. The tool mounts the PARENT of the
// output directory instead, and the container user removes and recreates the leaf like any directory.
//
// Exit codes: the exit code of the container (the spec's), 2 for a usage or environment error.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	imageEnv     = "KOINON_DRILL_PLAYWRIGHT_IMAGE"
	outputDirEnv = "PLAYWRIGHT_OUTPUT_DIR"
	usageError   = 2
)

// passedEnv selects the variables handed to the container, by name.
var passedEnv = regexp.MustCompile(`^(KOINON_DRILL_|PLAYWRIGHT_OUTPUT_DIR$|HTTPS?_PROXY$|NO_PROXY$|https?_proxy$|no_proxy$)`)

// config is everything the docker command line depends on.
type config struct {
	Repo      string   // absolute repository root, mounted at the same path
	Image     string   // the pinned Playwright image, by digest
	Spec      string   // spec name, passed to playwright test
	OutputDir string   // PLAYWRIGHT_OUTPUT_DIR; empty when unset
	UID, GID  int      // the container user
	Environ   []string // KEY=VALUE pairs of the host environment
}

// envNames returns the sorted names of the variables to pass to the container.
func envNames(environ []string) []string {
	var names []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if passedEnv.MatchString(name) && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// within reports whether path is dir or below it.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// mountDirs returns the host directories to bind-mount at the same path. The output directory's parent
// joins the list unless the repository mount already covers it (a duplicate mount point is a docker error).
func mountDirs(c config) []string {
	dirs := []string{c.Repo}
	if c.OutputDir != "" {
		if parent := filepath.Dir(filepath.Clean(c.OutputDir)); !within(c.Repo, parent) {
			dirs = append(dirs, parent)
		}
	}
	return dirs
}

// dockerArgs builds the argument list after `docker`.
func dockerArgs(c config) ([]string, error) {
	if c.Spec == "" {
		return nil, errors.New("usage: drillwebrun <spec>")
	}
	if !strings.Contains(c.Image, "@sha256:") {
		return nil, fmt.Errorf("the image must be pinned by digest: %q", c.Image)
	}
	if c.OutputDir != "" && !filepath.IsAbs(c.OutputDir) {
		return nil, fmt.Errorf("%s must be an absolute path: %q", outputDirEnv, c.OutputDir)
	}
	args := []string{
		"run", "--rm", "--init", "--ipc=host", "--network=host",
		"--user", fmt.Sprintf("%d:%d", c.UID, c.GID), "-e", "HOME=/tmp",
	}
	for _, name := range envNames(c.Environ) {
		args = append(args, "-e", name)
	}
	for _, d := range mountDirs(c) {
		args = append(args, "-v", d+":"+d)
	}
	return append(args, "-w", filepath.Join(c.Repo, "web"), c.Image,
		"node_modules/.bin/playwright", "test", "-c", "playwright.drill.config.ts", c.Spec), nil
}

// findRepo walks up from dir to the directory with go.mod.
func findRepo(dir string) (string, error) {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		}
		if filepath.Dir(d) == d {
			return "", fmt.Errorf("no go.mod above %s: run drillwebrun inside the repository", dir)
		}
	}
}

// run executes the tool and returns its exit code. docker is the executable to start.
func run(args, environ []string, wd, docker string, stdout, stderr io.Writer) int {
	var spec string
	if len(args) == 1 {
		spec = args[0]
	}
	var image, outDir string
	for _, kv := range environ {
		switch k, v, _ := strings.Cut(kv, "="); k {
		case imageEnv:
			image = v
		case outputDirEnv:
			outDir = v
		}
	}
	if image == "" {
		_, _ = fmt.Fprintf(stderr, "drillwebrun: %s is not set (the pinned Playwright image, by digest)\n", imageEnv)
		return usageError
	}
	repo, err := findRepo(wd)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "drillwebrun:", err)
		return usageError
	}
	c := config{Repo: repo, Image: image, Spec: spec, OutputDir: outDir, UID: os.Getuid(), GID: os.Getgid(), Environ: environ}
	dargs, err := dockerArgs(c)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "drillwebrun:", err)
		return usageError
	}
	if outDir != "" {
		if err := os.MkdirAll(filepath.Dir(outDir), 0o750); err != nil {
			_, _ = fmt.Fprintln(stderr, "drillwebrun:", err)
			return usageError
		}
	}
	cmd := exec.CommandContext(context.Background(), docker, dargs...) //nolint:gosec // G204: docker argv is built by dockerArgs from validated config
	cmd.Env, cmd.Stdout, cmd.Stderr = environ, stdout, stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		_, _ = fmt.Fprintln(stderr, "drillwebrun:", err)
		return usageError
	}
	return 0
}

func main() {
	wd, err := os.Getwd()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "drillwebrun:", err)
		os.Exit(usageError)
	}
	os.Exit(run(os.Args[1:], os.Environ(), wd, "docker", os.Stdout, os.Stderr))
}
