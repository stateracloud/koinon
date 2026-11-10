// SPDX-License-Identifier: Apache-2.0

package drill

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// SeededPassword is the fixed fake password the seed gives the bootstrap admin.
const SeededPassword = "koinon_test_not-a-real-password"

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

// ValuesOverlay is the part of the Koinon chart values that differs from the base file for one
// install: the fake model's host in the egress policy, the install's own gateway, its gateway ID and its client TLS
// Secret, and the hold. The base file is the kind quickstart's (test/e2e/kind/koinon-values.yaml).
func ValuesOverlay(fakesNS string, gw Gateway, gatewayID string, hold bool, storageClass string) (string, error) {
	pg := map[string]any{"cnpg": map[string]any{"storage": map[string]any{"size": "1Gi"}}}
	if storageClass != "" {
		pg["cnpg"].(map[string]any)["storage"].(map[string]any)["storageClass"] = storageClass
	}
	v := map[string]any{
		"restoreHold": hold,
		"postgres":    pg,
		"egress":      map[string]any{"allowedInternalHosts": []string{fmt.Sprintf("fakemodel.%s.svc:8080", fakesNS)}},
		"networkPolicy": map[string]any{"gatewayEgressTo": []any{map[string]any{
			"to":    []any{map[string]any{"namespaceSelector": map[string]any{"matchLabels": map[string]string{"kubernetes.io/metadata.name": fakesNS}}}},
			"ports": []any{map[string]any{"port": 8080}},
		}}},
		"openshell": map[string]any{"address": gw.Address(), "clientTLSSecret": gw.ClientTLSSecret, "gatewayId": gatewayID},
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		return "", err
	}
	return "# SPDX-License-Identifier: Apache-2.0\n" + string(b), nil
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
    %[3]s: %[2]s
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: fakemodel-scripts
  namespace: %[1]s
  labels:
    %[3]s: %[2]s
data:
  drill.yaml: |
%[5]s
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: fakemodel
  namespace: %[1]s
  labels: {app: fakemodel, %[3]s: %[2]s}
spec:
  replicas: 1
  selector:
    matchLabels: {app: fakemodel}
  template:
    metadata:
      labels: {app: fakemodel, %[3]s: %[2]s}
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
    %[3]s: %[2]s
spec:
  selector: {app: fakemodel}
  ports:
    - {name: http, port: 8080, targetPort: http}
`, ns, runID, RunLabel, image, indented)
}
