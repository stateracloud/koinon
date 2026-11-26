// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/testing/testenv"
	"github.com/stateracloud/koinon/internal/testing/uniqname"
)

// volumeCopy is met when a file that fillVolume writes into a Docker volume is readable from a
// second container that mounts the volume. Every pod volume of this suite (ServiceAccount token,
// Secrets, ConfigMaps) is made that way, and a runtime where it does not work gives hook
// containers that fail with "open /var/run/secrets/.../token: no such file or directory": a
// failure that looks like a chart bug. The check names the runtime feature instead (issue 1242).
var volumeCopy = testenv.Check("volumecopy", func() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	docker, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer func() { _ = docker.Close() }()
	return probeVolumeCopy(ctx, docker, pgtest.Image)
})

func probeVolumeCopy(ctx context.Context, docker *testcontainers.DockerClient, image string) error {
	const (
		want       = "koinon_test_not-a-real-token"
		probeGroup = 4242
	)
	name := uniqname.Name("chartsmoke-probe")
	defer func() {
		_, _ = docker.VolumeRemove(context.Background(), name, client.VolumeRemoveOptions{Force: true})
	}()
	if err := fillVolume(ctx, docker, image, name, map[string]volumeFile{"probe/token": {[]byte(want), 0o640}}, probeGroup); err != nil {
		return err
	}
	reader, err := testcontainers.Run(ctx, image,
		testcontainers.WithEntrypoint("cat"), testcontainers.WithConfigModifier(func(c *container.Config) { c.User = "65534:4242" }), testcontainers.WithCmd("/x/probe/token"),
		testcontainers.WithMounts(testcontainers.VolumeMount(name, "/x")),
		testcontainers.WithWaitStrategy(wait.ForExit()))
	if reader != nil {
		defer func() { terminate(reader) }()
	}
	if err != nil {
		return fmt.Errorf("read the volume back: %w", err)
	}
	logs, err := reader.Logs(ctx)
	if err != nil {
		return fmt.Errorf("read the volume back: %w", err)
	}
	defer func() { _ = logs.Close() }()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := logs.Read(buf)
		sb.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	if !strings.Contains(sb.String(), want) {
		return fmt.Errorf("the container runtime does not copy files into the volume of a running container "+
			"(copy to container, set the group, then read as a non-root user of that group from a second container): the file is missing or not readable by the group, read %q; "+
			"the pod volumes of this suite would be empty", strings.TrimSpace(sb.String()))
	}
	return nil
}
