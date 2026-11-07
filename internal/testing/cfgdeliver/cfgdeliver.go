// SPDX-License-Identifier: Apache-2.0

// Package cfgdeliver delivers an agentgateway config into a test container through the Docker API
// (#301).
//
// agentgateway reloads its config on an inotify watch. On macOS with OrbStack a host-side write into
// a bind mount produces no inotify event in the Linux VM, so a test that rewrites a bind-mounted
// config.yaml never sees the reload (the production FileDeliverer is correct on Linux). A copy through
// the Docker API happens inside the VM and does produce the event, so tests deliver that way.
package cfgdeliver

import (
	"bytes"
	"context"
	"fmt"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"

	"github.com/stateracloud/koinon/internal/gateway"
)

// ConfigDir is the directory the config lives in inside the container; Path is the config file.
const (
	ConfigDir = "/cfg"
	Path      = ConfigDir + "/config.yaml"
)

// ContainerOptions return the options that put first at [Path] before the container starts. /cfg is an
// anonymous volume, so it exists in an image that has no such directory and lives inside the VM.
func ContainerOptions(first []byte) []testcontainers.ContainerCustomizer {
	return []testcontainers.ContainerCustomizer{
		testcontainers.WithCmd("-f", Path),
		testcontainers.WithConfigModifier(func(c *container.Config) {
			if c.Volumes == nil {
				c.Volumes = map[string]struct{}{}
			}
			c.Volumes[ConfigDir] = struct{}{}
		}),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader:            bytes.NewReader(first),
			ContainerFilePath: Path,
			FileMode:          0o644,
		}),
	}
}

// Deliverer copies each config into a running container. It satisfies standalone.Deliverer.
//
// The write is not atomic: Docker's archive extraction unlinks and recreates config.yaml, unlike
// FileDeliverer's temp-file-plus-rename. agentgateway's watch debounce is what covers a partial read.
type Deliverer struct {
	Container testcontainers.Container
}

// Deliver writes config to [Path].
func (d Deliverer) Deliver(ctx context.Context, config []byte, _ gateway.Revision) error {
	if err := d.Container.CopyToContainer(ctx, config, Path, 0o644); err != nil {
		return fmt.Errorf("copy config to %s: %w", Path, err)
	}
	return nil
}
