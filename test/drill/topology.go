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
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/stateracloud/koinon/internal/project"
)

// The two drill profiles (ADR 0109 decision 2).
const (
	ProfileKind = "kind"
	ProfileLab  = "lab"
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

// OIDC is the external identity provider the restored copy must accept (`oidc-signin`). The lab has a standing
// public client for it; the chart's oidc values are set from these fields on both installs, so the source and the
// restored copy trust the same issuer.
type OIDC struct {
	// IssuerURL is the provider's issuer (https). ClientID is the standing client the drill uses.
	IssuerURL string `yaml:"issuerUrl" json:"issuerUrl"`
	ClientID  string `yaml:"clientId" json:"clientId"`
	// ClientSecretName is the Secret (key client-secret) in each install's namespace that holds the client's
	// secret; empty means a public client. The operator's namespaceHook creates it (ExternalSecret or other).
	ClientSecretName string `yaml:"clientSecretName,omitempty" json:"clientSecretName,omitempty"`
	// RedirectURL is the callback the client allows. The browser reaches the restored API through a
	// port-forward on localhost, so the client must allow this URL; the default is
	// http://localhost:<port>/auth/oidc/callback with the fixed port below.
	RedirectURL string `yaml:"redirectUrl,omitempty" json:"redirectUrl,omitempty"`
	// LocalPort is the fixed local port the drill forwards the restored API to for this check, so the
	// redirect URL of the client is stable. Default 18080.
	LocalPort int `yaml:"localPort,omitempty" json:"localPort,omitempty"`
}

// The environment variables of the `oidc-signin` check. They carry the drill user's name and the virtual
// authenticator's credential (a base64 PKCS#8 key and a credential ID the lab registered for that user at the
// provider); the values live in the operator's environment and never in a file of the repository.
const (
	EnvOIDCUser          = "KOINON_DRILL_OIDC_USER"
	EnvOIDCCredentialID  = "KOINON_DRILL_OIDC_CREDENTIAL_ID"
	EnvOIDCPrivateKey    = "KOINON_DRILL_OIDC_PRIVATE_KEY"
	EnvOIDCUserHandle    = "KOINON_DRILL_OIDC_USER_HANDLE"
	EnvOIDCRPID          = "KOINON_DRILL_OIDC_RP_ID"
	DefaultOIDCLocalPort = 18080
)

// Topology says where a drill runs: kind in CI, the home lab before a final tag (a lab file stays
// outside the repository, only this schema is in it).
type Topology struct {
	Name string `yaml:"name" json:"name"`
	// Profile is "kind" (CI, the gate of an rc tag) or "lab" (the home lab, the gate of a final tag). The
	// release gate (hack/release-gate.sh) reads it from drill-result.json, so a kind result cannot stand in
	// for a lab one whatever the file is called.
	Profile string `yaml:"profile" json:"profile"`
	// BrowserSignin adds the `browser-signin` check: a Playwright spec signs in through the web UI on the
	// restored copy (web/e2e-drill). Needs pnpm and a Chromium for Playwright on the machine that runs the
	// drill. The lab profile turns it on; kind does not (ADR 0109: a browser for every check is too slow).
	BrowserSignin bool `yaml:"browserSignin,omitempty" json:"browserSignin,omitempty"`
	// Context is the kube context. The kubeconfig is never read from here: the run takes KUBECONFIG
	// from its environment (run.sh refuses an empty one).
	Context string  `yaml:"context" json:"context"`
	Source  Install `yaml:"source" json:"source"`
	Target  Install `yaml:"target" json:"target"`
	// NamespaceHook is a manifest file the operator supplies (a path relative to the repository root, or
	// absolute; a lab keeps it outside the repository). After the drill creates each of its namespaces it
	// applies the file there with `kubectl apply -n <namespace>`, so the operator can add the access objects the
	// topology needs (for example ExternalSecrets that bring a credential in). The repository carries the
	// mechanism only: no lab endpoint or store is named in it. Objects in the file must not name a namespace
	// of their own (kubectl refuses a mismatch). The namespace goes away with the objects at the end of the run.
	NamespaceHook string `yaml:"namespaceHook,omitempty" json:"namespaceHook,omitempty"`
	// NamespaceLabels are added to every namespace the drill creates, before the hook is applied (a lab
	// selects namespaces by them, for example to let a secret store serve them).
	NamespaceLabels map[string]string `yaml:"namespaceLabels,omitempty" json:"namespaceLabels,omitempty"`
	// OIDC describes the external identity provider of the `oidc-signin` check. The check exists only when it
	// is set. The issuer and the client are inputs of the topology; the user's credentials are never in the
	// file (OIDCEnv names the environment variables they come from).
	OIDC *OIDC `yaml:"oidc,omitempty" json:"oidc,omitempty"`
	// FakesNamespace holds the drill's fake model server and its object store.
	FakesNamespace string `yaml:"fakesNamespace" json:"fakesNamespace"`
	// ChartPath is the chart under test; ValuesFile the base values (relative to the repository root, or absolute).
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
	// ImageRegistry, when set, is passed to the chart as image.registry. Only run.sh writes it, for the kind
	// topology copy: the cluster's path on the shared local registry (test/e2e/kind/imagerepo.sh).
	ImageRegistry string `yaml:"imageRegistry,omitempty" json:"imageRegistry,omitempty"`
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

// Path resolves a file named in the topology: relative to the repository root, or as it is when absolute (a
// lab values file kept outside the repository).
func (t *Topology) Path(repoRoot, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(repoRoot, p)
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
	if t.Profile != ProfileKind && t.Profile != ProfileLab {
		return fmt.Errorf("profile %q is neither %q nor %q: the release gate tells the two results apart by it", t.Profile, ProfileKind, ProfileLab)
	}
	if t.Profile == ProfileLab && !t.BrowserSignin {
		return fmt.Errorf("the lab profile needs browserSignin: true (#518: the lab gate includes the browser sign-in)")
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
	for k := range t.NamespaceLabels {
		if k == RunLabel {
			return fmt.Errorf("namespaceLabels %q is the drill's own run label", k)
		}
		if !labelKeyRE.MatchString(k) {
			return fmt.Errorf("namespaceLabels key %q is not a valid label key", k)
		}
	}
	for k, v := range t.NamespaceLabels {
		if !labelValueRE.MatchString(v) {
			return fmt.Errorf("namespaceLabels %q: value %q is not a valid label value", k, v)
		}
	}
	if t.OIDC != nil {
		if err := t.OIDC.validate(); err != nil {
			return fmt.Errorf("oidc: %w", err)
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

var (
	labelKeyRE   = regexp.MustCompile(`^([a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?/)?[A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$`)
	labelValueRE = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?)?$`)
)

func (o *OIDC) validate() error {
	if !strings.HasPrefix(o.IssuerURL, "https://") {
		return fmt.Errorf("issuerUrl %q must be an https URL", o.IssuerURL)
	}
	if o.ClientID == "" {
		return fmt.Errorf("clientId is empty")
	}
	if o.LocalPort < 0 || o.LocalPort > 65535 {
		return fmt.Errorf("localPort %d is not a port", o.LocalPort)
	}
	if o.RedirectURL != "" && !strings.HasPrefix(o.RedirectURL, "http") {
		return fmt.Errorf("redirectUrl %q must be an http(s) URL", o.RedirectURL)
	}
	return nil
}

// Port is the local port of the forward, with the default applied.
func (o *OIDC) Port() int {
	if o.LocalPort == 0 {
		return DefaultOIDCLocalPort
	}
	return o.LocalPort
}

// Redirect is the callback URL, with the default applied.
func (o *OIDC) Redirect() string {
	if o.RedirectURL != "" {
		return o.RedirectURL
	}
	return fmt.Sprintf("http://localhost:%d/auth/oidc/callback", o.Port())
}

// NamespaceManifest is the Namespace object the drill creates: the run label, then the operator's labels.
func NamespaceManifest(ns, runID string, labels map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n  labels:\n    %s: %q\n", ns, RunLabel, runID)
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "    %s: %q\n", k, labels[k])
	}
	return b.String()
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
