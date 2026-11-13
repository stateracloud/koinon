// SPDX-License-Identifier: Apache-2.0

package drill

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// SeededPassword is the fixed fake password the seed gives the bootstrap admin.
const SeededPassword = "koinon_test_not-a-real-password"

// The local users the seed adds (#927): an org admin, a member and a break-glass target (a member), all of the default org with SeededPassword.
const (
	OrgAdminUser = "drill-admin"
	MemberUser   = "drill-member"
	// BreakGlassUser is a third local user (a member) that only the recovery-signin check resets, so the
	// password of the others stays what the seed set.
	BreakGlassUser = "drill-breakglass"
	// ApprovalTask is the message that makes the fake model call the fake MCP server's ask-tier write tool.
	ApprovalTask = "drill-approval: please call the tracker's write tool."
	// ApprovalTool is that tool as the agent sees it (<org>-<server>-ask_<tool>).
	ApprovalTool = "default-tracker-ask_write_item"
)

// SeededProviderKey is the fake provider key the seed enters; the fake model must see it again
// after the restore.
const SeededProviderKey = "koinon_test_not-a-real-key"

// ModelName is the fake model's name (fakemodel/drill.yaml).
const ModelName = "drill"

// TriggerID is the id of the seeded agent's schedule trigger.
const TriggerID = "every-minute"

// FakeModelURL is the provider URL the seed enters.
func FakeModelURL(fakesNS string) string {
	return fmt.Sprintf("http://fakemodel.%s.svc:8080/v1", fakesNS)
}

// FakeMCPURL is the URL the seed registers the fake MCP server under.
func FakeMCPURL(fakesNS string) string {
	return fmt.Sprintf("http://fakemcp.%s.svc:8080/mcp", fakesNS)
}

// Fake MCP server credential: the header the gateway injects and its (obviously fake) value.
const (
	FakeMCPHeader = "X-Api-Key"
	FakeMCPKey    = "koinon_test_not-a-real-token"
)

// ValuesOverlay is the part of the Koinon chart values that differs from the base file for one
// install: the fake model's host in the egress policy, the install's own gateway, its gateway ID and its client TLS
// Secret, and the hold. The base file is the kind quickstart's (test/e2e/kind/koinon-values.yaml).
//
// cp turns home checkpoints on (ADR 0032); a nil cp leaves them off. oidc, when set, turns the chart's OIDC login on
// for the topology's issuer and client, with the redirect URL on oidcPort (the fixed local port of that side).
func ValuesOverlay(fakesNS string, gw Gateway, gatewayID string, hold bool, storageClass string, cp *Checkpoints, oidc *OIDC, oidcPort int) (string, error) {
	pg := map[string]any{"cnpg": map[string]any{"storage": map[string]any{"size": "1Gi"}}}
	if storageClass != "" {
		pg["cnpg"].(map[string]any)["storage"].(map[string]any)["storageClass"] = storageClass
	}
	// The sandbox admission policies name the OpenShell gateway's ServiceAccount; the chart's defaults name
	// the shared install's (openshell:openshell), so this gateway's sandboxes would be denied.
	gwSA := fmt.Sprintf("system:serviceaccount:%s:%s", gw.Namespace, gw.Service)
	v := map[string]any{
		"sandboxPolicy": map[string]any{"identities": map[string]any{
			"supervisorPods": []string{gwSA},
			"sandboxes":      []string{gwSA, "system:serviceaccount:agent-sandbox-system:agent-sandbox-controller"},
			"secrets":        []string{gwSA},
			"namespaces":     []string{gwSA},
		}},
		// Schedule triggers, Run now and the workflow queue all run on the DBOS runtime (off by default); the
		// drill's schedule, catch-up and ENQUEUED checks need it.
		"api":         map[string]any{"durable": map[string]any{"enabled": true}},
		"restoreHold": hold,
		"postgres":    pg,
		"egress":      map[string]any{"allowedInternalHosts": []string{fmt.Sprintf("fakemodel.%s.svc:8080", fakesNS), fmt.Sprintf("fakemcp.%s.svc:8080", fakesNS)}},
		"networkPolicy": map[string]any{"gatewayEgressTo": []any{map[string]any{
			"to":    []any{map[string]any{"namespaceSelector": map[string]any{"matchLabels": map[string]string{"kubernetes.io/metadata.name": fakesNS}}}},
			"ports": []any{map[string]any{"port": 8080}},
		}}},
		"openshell": map[string]any{"address": gw.Address(), "clientTLSSecret": gw.ClientTLSSecret, "gatewayId": gatewayID},
		// The preflight reads the gateway's ConfigMap and workload by name; the chart's defaults name the
		// shared "openshell" install, whose gateway_id is "openshell". The gateway chart's fullname is its release name.
		"preflight": map[string]any{"openshell": map[string]any{"namespace": gw.Namespace, "configMap": gw.Service + "-config", "workload": gw.Service}},
	}
	if oidc != nil {
		// A public client: clientSecret.existingSecret stays empty (PKCE only). The groups map to roles in the
		// default org, so a persona keeps its role on the restored copy.
		v["oidc"] = map[string]any{
			"enabled": true, "issuerUrl": oidc.IssuerURL, "clientId": oidc.ClientID, "redirectUrl": OIDCRedirect(oidcPort),
			"groupMappings": map[string]string{oidc.Admin.Group: "admin", oidc.Member.Group: "member"},
		}
	}
	if cp != nil {
		v["checkpoints"] = map[string]any{"enabled": true, "s3": map[string]any{"existingSecret": CheckpointSecretName, "bucket": CheckpointBucket}}
		rec := map[string]any{}
		if cp.ArchiveAfter != "" {
			rec["archiveAfter"] = cp.ArchiveAfter
		}
		if cp.IdleTimeout != "" {
			rec["idleTimeout"] = cp.IdleTimeout
		}
		if len(rec) > 0 {
			v["reconciler"] = rec
		}
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		return "", err
	}
	return "# SPDX-License-Identifier: Apache-2.0\n" + string(b), nil
}

// Checkpoints turns on the home checkpoints of one install (phase 2). The source sets ArchiveAfter and
// IdleTimeout short, so an idle agent stops, takes its stop checkpoint and is archived within minutes; the
// target keeps the chart's defaults.
type Checkpoints struct {
	ArchiveAfter string // reconciler.archiveAfter, for example "1m"; empty keeps the chart's
	IdleTimeout  string // reconciler.idleTimeout, for example "2m"; empty keeps the chart's
}

const (
	// CheckpointSecretName is the Secret each install gets in its namespace for checkpoints.s3.existingSecret.
	CheckpointSecretName = "drill-checkpoints-s3"
	// CheckpointBucket is the bucket both installs use (each install's repositories live under orgs/<org>/homes/<agent>/).
	CheckpointBucket = "koinon-drill-checkpoints"
	// ObjectStoreName is the object store's Service and Deployment name in the fakes namespace.
	ObjectStoreName = "objectstore"
	// ObjectStorePort is the S3 port of the object store.
	ObjectStorePort = 8333
	// ObjectStoreRegion is the region the drill's S3 client uses.
	ObjectStoreRegion = "us-east-1"
	// ObjectStoreAccessKey and ObjectStoreSecretKey are the object store's only identity; obviously fake.
	ObjectStoreAccessKey = "koinon_test_not-a-real-key"
	ObjectStoreSecretKey = "koinon_test_not-a-real-secret"
)

// ObjectStoreEndpoint is the in-cluster S3 endpoint (plain HTTP, path-style).
func ObjectStoreEndpoint(fakesNS string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", ObjectStoreName, fakesNS, ObjectStorePort)
}

// ObjectStoreManifest is the drill's S3 server for home checkpoints (phase 2): SeaweedFS (Apache-2.0),
// the server the checkpoint writer's own test uses (internal/checkpoint/s3_test.go), with one fake
// identity, storage on an emptyDir, a Deployment and a Service in the (already labelled) fakes namespace.
// The data lives as long as the run: the source's snapshots are what the target restores from.
func ObjectStoreManifest(ns, runID, image string) string {
	identities := fmt.Sprintf(`{"identities":[{"name":"drill","credentials":[{"accessKey":%q,"secretKey":%q}],"actions":["Admin","Read","Write","List","Tagging"]}]}`,
		ObjectStoreAccessKey, ObjectStoreSecretKey)
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %[1]s-identities
  namespace: %[2]s
  labels:
    %[4]s: "%[3]s"
data:
  s3.json: |
    %[7]s
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
  labels: {app: %[1]s, %[4]s: "%[3]s"}
spec:
  replicas: 1
  strategy: {type: Recreate}
  selector:
    matchLabels: {app: %[1]s}
  template:
    metadata:
      labels: {app: %[1]s, %[4]s: "%[3]s"}
    spec:
      automountServiceAccountToken: false
      # The image's entrypoint, started as root, chowns /data and su-execs to its seaweed user
      # (uid 1000); with every capability dropped both fail and the container exits. Start as that
      # user: the entrypoint then skips the chown and the drop, and fsGroup makes the emptyDir its own.
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        runAsGroup: 1000
        fsGroup: 1000
      containers:
        - name: seaweedfs
          image: %[5]s
          imagePullPolicy: IfNotPresent
          args: ["server", "-s3", "-s3.config=/etc/seaweedfs/s3.json", "-s3.port=%[6]d", "-dir=/data", "-volume.max=8", "-master.volumeSizeLimitMB=64"]
          ports:
            - {name: s3, containerPort: %[6]d}
          readinessProbe:
            tcpSocket: {port: s3}
          volumeMounts:
            - {name: identities, mountPath: /etc/seaweedfs, readOnly: true}
            - {name: data, mountPath: /data}
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: [ALL]}
      volumes:
        - name: identities
          configMap: {name: %[1]s-identities}
        - name: data
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    %[4]s: "%[3]s"
spec:
  selector: {app: %[1]s}
  ports:
    - {name: s3, port: %[6]d, targetPort: s3}
`, ObjectStoreName, ns, runID, RunLabel, image, ObjectStorePort, identities)
}

// CheckpointSecretManifest is the Secret checkpoints.s3.existingSecret names (the chart's documented keys),
// in the Koinon install's namespace, pointing at the drill's object store.
func CheckpointSecretManifest(ns, runID, fakesNS string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
  labels:
    %s: "%s"
stringData:
  AWS_ACCESS_KEY_ID: %s
  AWS_SECRET_ACCESS_KEY: %s
  endpoint: %s
  region: %s
`, CheckpointSecretName, ns, RunLabel, runID, ObjectStoreAccessKey, ObjectStoreSecretKey, ObjectStoreEndpoint(fakesNS), ObjectStoreRegion)
}

// FakeModelManifest is the drill's fake model server: a labelled namespace, the script in a
// ConfigMap, a Deployment and a Service. The namespace carries the run label, so the sweep finds it.
func FakeModelManifest(ns, runID, image, script string) string {
	indented := "    " + strings.ReplaceAll(strings.TrimRight(script, "\n"), "\n", "\n    ")
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
  labels:
    %[3]s: "%[2]s"
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: fakemodel-scripts
  namespace: %[1]s
  labels:
    %[3]s: "%[2]s"
data:
  drill.yaml: |
%[5]s
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: fakemodel
  namespace: %[1]s
  labels: {app: fakemodel, %[3]s: "%[2]s"}
spec:
  replicas: 1
  selector:
    matchLabels: {app: fakemodel}
  template:
    metadata:
      labels: {app: fakemodel, %[3]s: "%[2]s"}
    spec:
      automountServiceAccountToken: false
      containers:
        - name: fakemodel
          image: %[4]s
          imagePullPolicy: IfNotPresent
          args: ["-addr", ":8080", "-scripts", "/scripts"]
          ports:
            - {name: http, containerPort: 8080}
          readinessProbe:
            tcpSocket: {port: http}
          volumeMounts:
            - {name: scripts, mountPath: /scripts, readOnly: true}
          securityContext:
            runAsNonRoot: true
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
      volumes:
        - name: scripts
          configMap: {name: fakemodel-scripts}
---
apiVersion: v1
kind: Service
metadata:
  name: fakemodel
  namespace: %[1]s
  labels:
    %[3]s: "%[2]s"
spec:
  selector: {app: fakemodel}
  ports:
    - {name: http, port: 8080, targetPort: http}
`, ns, runID, RunLabel, image, indented)
}

// FakeMCPManifest is the fake MCP server (cmd/koinon-fakemcp) in the fakes namespace. It serves
// read_item, write_item and fetch_url, and refuses a request without the credential header; the drill
// registers it with FakeMCPKey so the agent's ask-tier call parks for an approval.
func FakeMCPManifest(ns, runID, image string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: fakemcp
  namespace: %[1]s
  labels: {app: fakemcp, %[3]s: "%[2]s"}
spec:
  replicas: 1
  selector:
    matchLabels: {app: fakemcp}
  template:
    metadata:
      labels: {app: fakemcp, %[3]s: "%[2]s"}
    spec:
      automountServiceAccountToken: false
      containers:
        - name: fakemcp
          image: %[4]s
          imagePullPolicy: IfNotPresent
          args: ["-addr", ":8080", "-header", "%[5]s", "-value", "%[6]s"]
          ports:
            - {name: http, containerPort: 8080}
          readinessProbe:
            httpGet: {path: /healthz, port: http}
          securityContext:
            runAsNonRoot: true
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
---
apiVersion: v1
kind: Service
metadata:
  name: fakemcp
  namespace: %[1]s
  labels:
    %[3]s: "%[2]s"
spec:
  selector: {app: fakemcp}
  ports:
    - {name: http, port: 8080, targetPort: http}
`, ns, runID, RunLabel, image, FakeMCPHeader, FakeMCPKey)
}
