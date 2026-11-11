// SPDX-License-Identifier: Apache-2.0

// Package drill holds the pieces of the restore drill (ADR 0109) that need no cluster: the topology
// file, the guide-block substitution, the run label, the result file and the RTO and RPO arithmetic.
// The specs that use them are in this directory's drill_test package.
package drill

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/stateracloud/koinon/internal/project"
)

// NamespacePrefix starts every namespace the drill creates or touches. The sweep and the topology
// check refuse anything else, so a wrong KUBECONFIG cannot make the drill delete a real namespace.
const NamespacePrefix = "koinon-drill-"

// RunLabel is the label every object of one drill run carries; its value is the run ID.
const RunLabel = project.Domain + "/drill-run"

// Install is one Koinon install and the OpenShell gateway it uses. {run} in a name is replaced by
// the run ID.
type Install struct {
	Namespace        string `yaml:"namespace" json:"namespace"`
	Release          string `yaml:"release" json:"release"`
	GatewayNamespace string `yaml:"gatewayNamespace" json:"gatewayNamespace"`
	GatewayRelease   string `yaml:"gatewayRelease" json:"gatewayRelease"`
	// GatewayID is the gateway's gateway_id (its chart's gatewayId, Koinon's openshell.gatewayId). In
	// managed mode OpenShell names workspace namespaces openshell-<gatewayId>-<workspace>, and the
	// restored org has the same workspace as the source, so two gateways with one ID would manage the
	// same namespace. The ID is per run and per side.
	GatewayID string `yaml:"gatewayId" json:"gatewayId"`
}

// Topology says where a drill runs: kind in CI, the home lab before a final tag (a lab file stays
// outside the repository, only this schema is in it).
type Topology struct {
	Name string `yaml:"name" json:"name"`
	// Context is the kube context. The kubeconfig is never read from here: the run takes KUBECONFIG
	// from its environment (run.sh refuses an empty one).
	Context string  `yaml:"context" json:"context"`
	Source  Install `yaml:"source" json:"source"`
	Target  Install `yaml:"target" json:"target"`
	// FakesNamespace holds the drill's fake model server and its object store.
	FakesNamespace string `yaml:"fakesNamespace" json:"fakesNamespace"`
	// ChartPath is the chart under test; ValuesFile the base values (relative to the repository root).
	ChartPath  string `yaml:"chartPath" json:"chartPath"`
	ValuesFile string `yaml:"valuesFile" json:"valuesFile"`
	// Gateway chart: an OCI reference with its version, and the values file for it.
	GatewayChart   string `yaml:"gatewayChart" json:"gatewayChart"`
	GatewayVersion string `yaml:"gatewayVersion" json:"gatewayVersion"`
	GatewayValues  string `yaml:"gatewayValues" json:"gatewayValues"`
	// FakeModelImage is the fake model server's image on this cluster.
	FakeModelImage string `yaml:"fakeModelImage" json:"fakeModelImage"`
	// ObjectStoreImage is the S3 server for home checkpoints (phase 2), pinned by digest: SeaweedFS, Apache-2.0.
	ObjectStoreImage string `yaml:"objectStoreImage" json:"objectStoreImage"`
	// Registry is the image registry of this topology (empty on kind: images are loaded into the node).
	Registry string `yaml:"registry,omitempty" json:"registry,omitempty"`
	// StorageClass for the databases; empty is the cluster default.
	StorageClass string `yaml:"storageClass,omitempty" json:"storageClass,omitempty"`
	// RunNowCalls is N: the number of Run-now calls; N-1 stay ENQUEUED behind the low trigger limits.
	RunNowCalls int `yaml:"runNowCalls" json:"runNowCalls"`
	// HeldObserve is how long the held checks watch the target for activity; it must span a schedule
	// slot of the `* * * * *` trigger.
	HeldObserve time.Duration `yaml:"heldObserve" json:"heldObserve"`
	// ReleasedWait bounds each wait of the released checks.
	ReleasedWait time.Duration `yaml:"releasedWait" json:"releasedWait"`
}

// LoadTopology reads and checks a topology file, and expands {run} with runID.
func LoadTopology(file, runID string) (*Topology, error) {
	b, err := os.ReadFile(file) //nolint:gosec // the topology path comes from the operator
	if err != nil {
		return nil, err
	}
	var t Topology
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	t.expand(runID)
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return &t, nil
}

func (t *Topology) expand(runID string) {
	r := strings.NewReplacer("{run}", runID)
	for _, p := range []*string{
		&t.Source.Namespace, &t.Source.Release, &t.Source.GatewayNamespace, &t.Source.GatewayRelease, &t.Source.GatewayID,
		&t.Target.Namespace, &t.Target.Release, &t.Target.GatewayNamespace, &t.Target.GatewayRelease, &t.Target.GatewayID,
		&t.FakesNamespace,
	} {
		*p = r.Replace(*p)
	}
}

// Validate checks the rules that keep a drill away from anything that is not its own.
func (t *Topology) Validate() error {
	if t.Context == "" {
		return fmt.Errorf("context is empty: a drill always names its kube context")
	}
	for name, ns := range map[string]string{
		"source.namespace": t.Source.Namespace, "source.gatewayNamespace": t.Source.GatewayNamespace,
		"target.namespace": t.Target.Namespace, "target.gatewayNamespace": t.Target.GatewayNamespace,
		"fakesNamespace": t.FakesNamespace,
	} {
		if err := CheckNamespace(ns); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if t.Source.Namespace == t.Target.Namespace {
		return fmt.Errorf("source and target use the same namespace %q: the target is a fresh one", t.Source.Namespace)
	}
	if t.Source.GatewayNamespace == t.Target.GatewayNamespace {
		return fmt.Errorf("source and target share the OpenShell gateway namespace %q: the target has its own gateway", t.Source.GatewayNamespace)
	}
	for name, id := range map[string]string{"source.gatewayId": t.Source.GatewayID, "target.gatewayId": t.Target.GatewayID} {
		if !gatewayIDRE.MatchString(id) {
			return fmt.Errorf("%s %q must start with %q (the sweep deletes the openshell-<gatewayId>-* namespaces of this run by it)", name, id, GatewayIDPrefix)
		}
	}
	if t.Source.GatewayID == t.Target.GatewayID {
		return fmt.Errorf("source and target share the gateway ID %q: both gateways would manage the same openshell-%s-<workspace> namespaces", t.Source.GatewayID, t.Source.GatewayID)
	}
	// The chart names objects <release> when the release contains "koinon" and <release>-koinon
	// otherwise; the guide substitution relies on the first.
	for name, rel := range map[string]string{"source.release": t.Source.Release, "target.release": t.Target.Release} {
		if !strings.Contains(rel, "koinon") {
			return fmt.Errorf("%s %q must contain \"koinon\": the chart's object names are the release name only then", name, rel)
		}
	}
	if t.Source.Release == t.Target.Release {
		return fmt.Errorf("source and target share the release name %q: cluster-scoped chart objects would collide", t.Source.Release)
	}
	if t.Source.GatewayRelease == "" || t.Target.GatewayRelease == "" || t.Source.GatewayRelease == t.Target.GatewayRelease {
		return fmt.Errorf("each install needs its own gateway release")
	}
	for name, v := range map[string]string{
		"chartPath": t.ChartPath, "valuesFile": t.ValuesFile, "gatewayChart": t.GatewayChart,
		"gatewayVersion": t.GatewayVersion, "gatewayValues": t.GatewayValues, "fakeModelImage": t.FakeModelImage,
		"objectStoreImage": t.ObjectStoreImage,
	} {
		if v == "" {
			return fmt.Errorf("%s is empty", name)
		}
	}
	if t.RunNowCalls < 3 {
		return fmt.Errorf("runNowCalls is %d: at least 3, so that at least 2 stay ENQUEUED and the queue check means something", t.RunNowCalls)
	}
	if t.HeldObserve < 90*time.Second {
		return fmt.Errorf("heldObserve is %s: it must span a slot of the every-minute schedule (at least 90s)", t.HeldObserve)
	}
	if t.ReleasedWait <= 0 {
		return fmt.Errorf("releasedWait is empty")
	}
	return nil
}

// GatewayIDPrefix starts every gateway ID the drill uses; the sweep refuses any other.
const GatewayIDPrefix = "drill-"

var gatewayIDRE = regexp.MustCompile(`^drill-[a-z0-9-]{1,40}$`)

var nsRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// CheckNamespace refuses a namespace that does not start with NamespacePrefix.
func CheckNamespace(ns string) error {
	if !strings.HasPrefix(ns, NamespacePrefix) || len(ns) == len(NamespacePrefix) {
		return fmt.Errorf("namespace %q does not start with %q: the drill touches only its own namespaces", ns, NamespacePrefix)
	}
	if !nsRE.MatchString(ns) {
		return fmt.Errorf("namespace %q is not a valid name", ns)
	}
	return nil
}

// NewRunID returns a short random run ID, safe in a namespace name and a label value.
func NewRunID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CheckRunID refuses a run ID that would not fit in names and labels.
func CheckRunID(id string) error {
	if !regexp.MustCompile(`^[a-z0-9]{3,12}$`).MatchString(id) {
		return fmt.Errorf("run ID %q must be 3 to 12 lowercase letters and digits", id)
	}
	return nil
}
