// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/stateracloud/koinon/internal/execution"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	"github.com/stateracloud/koinon/internal/guestauth"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/project"
)

// AgentContainer is the name of the one container of a Koinon actor template.
const AgentContainer = "agent"

// RuntimeClassSelector is the worker label that a SandboxSpec's RuntimeClassName selects. The
// workers of a pool carry it (the chart and the launcher slice set it, epic 1082 D6), and an
// ActorTemplate with the selector only lands on those workers. It is how an isolation tier
// (ADR 0075) reaches Substrate, which has pools and no runtime classes.
var RuntimeClassSelector = project.Domain + "/runtime-class"

// HomeVolume names the durable directory of every actor template, and HomeMountPath is where the
// agent container sees it. The path is the agent's home, the one the Koinon checkpoints save
// (checkpoint.HomeDir; the substrate package does not import checkpoint). A durable directory
// persists across suspend and resume and takes part in a snapshot at every fidelity, so the home
// survives an idle suspend without a Koinon checkpoint (issue #1090, maintainer decision 2026-11-25).
// It does not survive DeleteActor without a tag (lab, 2026-11-25): archive and restore stay with the
// Koinon kopia checkpoint, outside Substrate (ADR 0032, amended by this issue). A durable directory
// holds the home once in a memory snapshot, not twice.
const (
	HomeVolume    = "home"
	HomeMountPath = "/sandbox"
)

// GVisorCapAdd returns the capability set of the agent container on the gVisor tiers (hardened and
// dev; issue #1387, ADR 0120 amendment of 2026-11-26). Every capability is dropped, then these are
// added: SETUID and SETGID for the launcher's drop to the non-root user, SETPCAP for the drop of the
// bounding set before it, and KILL for the guest front, which signals the processes it starts. The
// payload keeps none of them: the launcher policy of the tier has an empty allowlist and requires the
// user drop, so a guest that cannot drop does not start the payload. The guest front keeps its
// listener on port 80 without NET_BIND_SERVICE: the gVisor netstack has no privileged-port check.
// This is true only while runsc runs with its own netstack (--network=sandbox, the runsc default);
// with --network=host the host kernel checks the port and the front needs NET_BIND_SERVICE.
// The vm tier keeps the Substrate default set until the lab proves it (issue #1462). Each call
// returns a new slice.
func GVisorCapAdd() []string { return []string{"KILL", "SETGID", "SETPCAP", "SETUID"} }

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
// share a template and a changed spec makes a new one. The template's one environment variable is
// the public key set that the guest front verifies the driver's tokens with (issue #1309); a public
// key is not a secret, so the guest still holds none (ADR 0119, ADR 0120 section 1). It fails
// without Config.GuestSigner: a template without keys would start a guest that refuses every call.
func renderTemplate(workspace string, spec execution.SandboxSpec, cfg Config) (*ateapiv1.ActorTemplate, error) {
	if spec.Name == "" {
		return nil, errors.New("sandbox name is empty")
	}
	if spec.Image == "" {
		return nil, errors.New("sandbox image is empty")
	}
	// The guest front is the image ENTRYPOINT and every call goes through it. Substrate takes a template
	// command as an override of ENTRYPOINT and CMD, so a Command would replace the front and every
	// call would fail with connection refused (issue #1092). Processes start through Exec.
	if len(spec.Command) > 0 {
		return nil, errors.New("sandbox command is set: it would replace the guest front (the image entrypoint); start processes through Exec")
	}
	// A tag can move; a digest cannot (issue #1192). The rewrite below keeps the digest.
	if err := RequireDigest(spec.Image); err != nil {
		return nil, err
	}
	cpu, mem, err := ResourceLimits(spec)
	if err != nil {
		return nil, err
	}
	keys, err := verifyKeys(cfg)
	if err != nil {
		return nil, err
	}
	// The node agent pulls from one image source: the registry of the reference is rewritten to it,
	// and the digest is kept (issue #1170). The rewritten reference is what the hash covers.
	image := RewriteImage(spec.Image, cfg.ImageRegistry, cfg.ImageMirrors)
	t := &ateapiv1.ActorTemplate{
		Metadata: &ateapiv1.ResourceMetadata{Atespace: workspace},
		Containers: []*ateapiv1.Container{{
			Name:         AgentContainer,
			Image:        image,
			Env:          []*ateapiv1.EnvVar{{Name: guestauth.VerifyKeysEnv, Value: keys}},
			VolumeMounts: []*ateapiv1.VolumeMount{{Name: HomeVolume, MountPath: HomeMountPath}},
			// The guest front answers /readyz on port 80 without a token. Substrate polls it on every
			// wakeup; a front that does not answer crashes the actor, and the crash path of issue #1481
			// reverts it.
			WakeupProbe: &ateapiv1.WakeupProbe{
				HttpGet:        &ateapiv1.HttpGetAction{Path: FrontProbePath, Port: FrontProbePort},
				TimeoutSeconds: FrontProbeTimeoutSeconds,
			},
		}},
		Volumes: []*ateapiv1.Volume{{Name: HomeVolume, DurableDir: &ateapiv1.DurableDirVolumeSource{}}},
		SnapshotConfig: &ateapiv1.SnapshotConfig{
			StorageLocation:   cfg.SnapshotStorageLocation,
			PreferredFidelity: cfg.SnapshotFidelity,
			// Substrate builds golden snapshots when this config is omitted, so the mode is always set.
			GoldenSnapshotConfig: &ateapiv1.GoldenSnapshotConfig{Mode: goldenMode(cfg.GoldenSnapshots)},
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
	var capAdd []string // nil: the Substrate default set
	var capDrop []string
	if templateTier(spec, cfg).GVisor() {
		capDrop, capAdd = []string{"ALL"}, GVisorCapAdd()
		t.Containers[0].SecurityContext = &ateapiv1.SecurityContext{Capabilities: &ateapiv1.Capabilities{Drop: capDrop, Add: capAdd}}
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "v6\x00%s\x00%s\x00%q\x00%d\x00%d\x00%s\x00%s\x00%d\x00%s\x00%s:%s\x00%q\x00%q\x00%t", workspace, image, spec.Command, cpu, mem,
		spec.RuntimeClassName, cfg.SnapshotStorageLocation, cfg.SnapshotFidelity, keys, HomeVolume, HomeMountPath, capDrop, capAdd, cfg.GoldenSnapshots)
	t.Metadata.Name = templatePrefix + hex.EncodeToString(h.Sum(nil))[:20]
	return t, nil
}

// The wakeup probe of the guest front (issue #1481). The front serves /readyz on port 80 (package guestfront).
const (
	FrontProbePath           = "/readyz"
	FrontProbePort           = 80
	FrontProbeTimeoutSeconds = 30
)

// goldenMode is the template's golden snapshot mode: disabled unless an admin turns it on (issue #1492).
func goldenMode(on bool) ateapiv1.GoldenSnapshotMode {
	if on {
		return ateapiv1.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED
	}
	return ateapiv1.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED
}

// templateTier is the isolation tier of a spec, resolved the way Exec resolves it from the template:
// the tier of the runtime class, or Config.DefaultTier without one. An unknown class has no tier here;
// Exec then refuses every process of the sandbox.
func templateTier(spec execution.SandboxSpec, cfg Config) harness.Tier {
	if spec.RuntimeClassName == "" {
		return cfg.DefaultTier
	}
	return cfg.RuntimeClassTiers[spec.RuntimeClassName]
}

// verifyKeys returns the value of guestauth.VerifyKeysEnv: the signer's public key first, then
// Config.GuestVerifyKeys without repeats.
func verifyKeys(cfg Config) (string, error) {
	if cfg.GuestSigner == nil {
		return "", errors.New("no guest API signing key (issue #1309)")
	}
	keys := []ed25519.PublicKey{cfg.GuestSigner.PublicKey()}
	for _, k := range cfg.GuestVerifyKeys {
		if !slices.ContainsFunc(keys, func(have ed25519.PublicKey) bool { return have.Equal(k) }) {
			keys = append(keys, k)
		}
	}
	return guestauth.EncodeKeys(keys...), nil
}

// IsDerivedTemplate reports whether a template name was made by the driver, for the cleanup that a
// later slice adds.
func IsDerivedTemplate(name string) bool { return strings.HasPrefix(name, templatePrefix) }
