// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// koinonImages are the binaries the chart runs in images that Koinon builds. The chart's package
// names are shorter (<image.registry>/api, /reconciler, /agentd; #167), see imageBinary. A new
// Koinon image in the chart that is not listed here fails the suite, so a new component cannot slip
// past the smoke test.
var koinonImages = []string{"koinon-api", "koinon-agentd", "koinon-reconciler"}

// imageBinary maps the last path element of a Koinon image reference to its binary.
var imageBinary = map[string]string{"api": "koinon-api", "agentd": "koinon-agentd", "reconciler": "koinon-reconciler"}

// imageName is the binary of a Koinon image reference without tag or digest, else the last path
// element of the reference.
func imageName(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	ref = ref[strings.LastIndex(ref, "/")+1:]
	name, _, _ := strings.Cut(ref, ":")
	if bin, ok := imageBinary[name]; ok {
		return bin
	}
	return name
}

// envWithoutTestingTag drops the `testing` build tag from GOFLAGS (hack/go-test.sh sets
// GOFLAGS=-tags=testing for test binaries, #576) and keeps every other flag and tag. The images
// are what a release build produces, and the tag makes kopia derive keys insecurely.
func envWithoutTestingTag(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		val, ok := strings.CutPrefix(kv, "GOFLAGS=")
		if !ok {
			out = append(out, kv)
			continue
		}
		var flags []string
		for _, f := range strings.Fields(val) {
			if tags, ok := strings.CutPrefix(f, "-tags="); ok {
				var keep []string
				for _, t := range strings.Split(tags, ",") {
					if t != "testing" && t != "" {
						keep = append(keep, t)
					}
				}
				if len(keep) == 0 {
					continue
				}
				f = "-tags=" + strings.Join(keep, ",")
			}
			flags = append(flags, f)
		}
		out = append(out, "GOFLAGS="+strings.Join(flags, " "))
	}
	return out
}

// buildImages compiles the Koinon binaries for the Docker daemon's architecture and wraps each in
// a scratch image that starts it as its entrypoint, as the ko images do (ko is not needed: the
// chart only cares about the binary and its arguments). It returns the local tag per image name.
func buildImages(ctx context.Context, root string) map[string]string {
	arch := dockerArch(ctx)
	tmp, err := os.MkdirTemp("", "koinon-chartsmoke-img-")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	tags := map[string]string{}
	for _, name := range koinonImages {
		bin := filepath.Join(tmp, name)
		build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", bin, "./cmd/"+name)
		build.Dir = root
		build.Env = append(envWithoutTestingTag(os.Environ()), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
		if out, err := build.CombinedOutput(); err != nil {
			panic(fmt.Sprintf("go build %s: %v\n%s", name, err, out))
		}
		sum := sha256.New()
		b, err := os.ReadFile(bin)
		if err != nil {
			panic(err)
		}
		sum.Write(b)
		tag := fmt.Sprintf("koinon-chartsmoke/%s:%x", name, sum.Sum(nil)[:6])

		dir := filepath.Join(tmp, name+".ctx")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o755); err != nil {
			panic(err)
		}
		dockerfile := fmt.Sprintf("FROM scratch\nCOPY %s /ko-app/%s\nENTRYPOINT [\"/ko-app/%s\"]\n", name, name, name)
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
			panic(err)
		}
		cmd := exec.CommandContext(ctx, "docker", "build", "-q", "--platform", "linux/"+arch, "-t", tag, dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			panic(fmt.Sprintf("docker build %s: %v\n%s", name, err, out))
		}
		tags[name] = tag
	}
	return tags
}

// removeImages removes the images buildImages made. An image another run is using stays (docker
// refuses), which is fine: that run removes it.
func removeImages(ctx context.Context, tags map[string]string) {
	for _, tag := range tags {
		_ = exec.CommandContext(ctx, "docker", "rmi", tag).Run()
	}
}

// dockerArch is the Go architecture name of the Docker daemon (a Linux VM on a Mac).
func dockerArch(ctx context.Context) string {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "info", "--format", "{{.Architecture}}")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return runtime.GOARCH
	}
	switch strings.TrimSpace(out.String()) {
	case "aarch64", "arm64":
		return "arm64"
	case "x86_64", "amd64":
		return "amd64"
	}
	return runtime.GOARCH
}
