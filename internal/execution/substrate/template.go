// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/stateracloud/koinon/internal/execution"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	"github.com/stateracloud/koinon/internal/project"
)

// AgentContainer is the name of the one container of a Koinon actor template.
const AgentContainer = "agent"

// RuntimeClassSelector is the worker label that a SandboxSpec's RuntimeClassName selects. The
// workers of a pool carry it (the chart and the launcher slice set it, epic 1082 D6), and an
// ActorTemplate with the selector only lands on those workers. It is how an isolation tier
// (ADR 0075) reaches Substrate, which has pools and no runtime classes.
var RuntimeClassSelector = project.Domain + "/runtime-class"

// templatePrefix starts the name of every template the driver derives.
const templatePrefix = "kt-"

// ResourceLimits maps a spec's CPU and memory to the one number Substrate has for each (ADR 0083,
// amended by ADR 0119). An ActorTemplate has limits only: the worker reserves them, so a request
// has no counterpart. The limit wins; a spec with a request and no limit gets the request as its
// limit, which is what an actor with that request needs at least. A missing value is zero, which
// leaves the template field unset.
func ResourceLimits(spec execution.SandboxSpec) (cpuMillis, memoryBytes int64, err error) {
	cpu := first(spec.CPULimit, spec.CPURequest)
	mem := first(spec.MemoryLimit, spec.MemoryRequest)
	if cpu != "" {
		q, perr := resource.ParseQuantity(cpu)
		if perr != nil || q.Sign() < 0 {
			return 0, 0, fmt.Errorf("cpu %q is not a quantity", cpu)
		}
		cpuMillis = q.MilliValue()
	}
	if mem != "" {
		q, perr := resource.ParseQuantity(mem)
		if perr != nil || q.Sign() < 0 {
			return 0, 0, fmt.Errorf("memory %q is not a quantity", mem)
		}
		memoryBytes = q.Value()
	}
	return cpuMillis, memoryBytes, nil
}

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// renderTemplate builds the ActorTemplate for a spec. It is a pure function of the spec and the
// driver's config, and its name is a hash of everything in it, so two sandboxes with equal specs
// share a template and a changed spec makes a new one. The template carries no environment
// variable: the guest sees no secret (ADR 0119, no credentials in the guest).
func renderTemplate(workspace string, spec execution.SandboxSpec, cfg Config) (*ateapiv1.ActorTemplate, error) {
	if spec.Name == "" {
		return nil, errors.New("sandbox name is empty")
	}
	if spec.Image == "" {
		return nil, errors.New("sandbox image is empty")
	}
	// A tag can move; a digest cannot (issue #1192). The rewrite below keeps the digest.
	if err := RequireDigest(spec.Image); err != nil {
		return nil, err
	}
	cpu, mem, err := ResourceLimits(spec)
	if err != nil {
		return nil, err
	}
	// The node agent pulls from one image source: the registry of the reference is rewritten to it,
	// and the digest is kept (issue #1170). The rewritten reference is what the hash covers.
	image := RewriteImage(spec.Image, cfg.ImageRegistry, cfg.ImageMirrors)
	t := &ateapiv1.ActorTemplate{
		Metadata: &ateapiv1.ResourceMetadata{Atespace: workspace},
		Containers: []*ateapiv1.Container{{
			Name:    AgentContainer,
			Image:   image,
			Command: spec.Command,
		}},
		SnapshotConfig: &ateapiv1.SnapshotConfig{
			StorageLocation:   cfg.SnapshotStorageLocation,
			PreferredFidelity: cfg.SnapshotFidelity,
		},
	}
	if cpu > 0 || mem > 0 {
		lim := &ateapiv1.Resources{CpuMillicores: cpu, MemoryBytes: mem}
		t.Resources = &ateapiv1.ResourceRequirements{Limits: lim}
		t.Containers[0].Resources = &ateapiv1.ResourceRequirements{Limits: lim}
	}
	if spec.RuntimeClassName != "" {
		t.WorkerSelector = &ateapiv1.Selector{MatchLabels: map[string]string{RuntimeClassSelector: spec.RuntimeClassName}}
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "v1\x00%s\x00%s\x00%q\x00%d\x00%d\x00%s\x00%s\x00%d", workspace, image, spec.Command, cpu, mem,
		spec.RuntimeClassName, cfg.SnapshotStorageLocation, cfg.SnapshotFidelity)
	t.Metadata.Name = templatePrefix + hex.EncodeToString(h.Sum(nil))[:20]
	return t, nil
}

// IsDerivedTemplate reports whether a template name was made by the driver, for the cleanup that a
// later slice adds.
func IsDerivedTemplate(name string) bool { return strings.HasPrefix(name, templatePrefix) }
