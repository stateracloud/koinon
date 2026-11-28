{{/* SPDX-License-Identifier: Apache-2.0 */}}

{{- define "koinon.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "koinon.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/* Name of a component's objects: koinon.componentName (list $ "api"). */}}
{{- define "koinon.componentName" -}}
{{- $ctx := index . 0 -}}
{{- printf "%s-%s" (include "koinon.fullname" $ctx) (index . 1) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "koinon.selectorLabels" -}}
{{- $ctx := index . 0 -}}
app.kubernetes.io/name: {{ include "koinon.name" $ctx }}
app.kubernetes.io/instance: {{ $ctx.Release.Name }}
app.kubernetes.io/component: {{ index . 1 }}
{{- end }}

{{- define "koinon.labels" -}}
{{- $ctx := index . 0 -}}
helm.sh/chart: {{ printf "%s-%s" $ctx.Chart.Name $ctx.Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "koinon.selectorLabels" . }}
app.kubernetes.io/version: {{ $ctx.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ $ctx.Release.Service }}
app.kubernetes.io/part-of: koinon
{{- end }}

{{/* Labels without a component, for cluster-scoped and shared objects. */}}
{{- define "koinon.commonLabels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "koinon.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: koinon
{{- end }}

{{/*
Rewrite an image reference for a registry mirror: koinon.imageRef (list $ "ghcr.io/org/name:tag@sha256:...").
The leading host is the first path element that contains "." or ":" (or is "localhost"); a reference
without one lives on docker.io (index.docker.io and registry-1.docker.io count as docker.io, and host names are compared
in lower case), and a bare "name" is docker.io/library/name. global.registryMirrors[host]
replaces that host if it has an entry; otherwise global.imageRegistry replaces it, if set; otherwise the
reference is returned exactly as given. A reference that already starts with imageRegistry or a mirror value is
returned as given too, so nothing is prefixed twice. The path, tag and digest are kept, so a digest still verifies
through a pull-through cache. Mirror values may carry a path prefix (art.example.com/ghcr-remote).
*/}}
{{- define "koinon.imageRef" -}}
{{- $ctx := index . 0 -}}
{{- $ref := index . 1 -}}
{{- $g := $ctx.Values.global | default dict -}}
{{- $mirrors := $g.registryMirrors | default dict -}}
{{- /* A reference that already lives under imageRegistry or a mirror value stays as it is: no double prefix. */ -}}
{{- $inside := false -}}
{{- range $t := concat (list ($g.imageRegistry | default "")) (values $mirrors) -}}
{{- if and $t (hasPrefix (printf "%s/" (trimSuffix "/" $t)) $ref) }}{{ $inside = true }}{{ end -}}
{{- end -}}
{{- if $inside -}}
{{- $ref -}}
{{- else -}}
{{- $parts := splitList "/" $ref -}}
{{- $host := "docker.io" -}}
{{- $rest := $ref -}}
{{- if and (gt (len $parts) 1) (or (contains "." (first $parts)) (contains ":" (first $parts)) (eq (lower (first $parts)) "localhost")) -}}
{{- /* Docker Hub has three names, and host names are not case sensitive. */ -}}
{{- $host = lower (first $parts) -}}
{{- if or (eq $host "index.docker.io") (eq $host "registry-1.docker.io") }}{{ $host = "docker.io" }}{{ end -}}
{{- $rest = join "/" (rest $parts) -}}
{{- /* Docker Hub has no API alias: docker.io/alpine is docker.io/library/alpine. */ -}}
{{- if and (eq $host "docker.io") (not (contains "/" $rest)) }}{{ $rest = printf "library/%s" $rest }}{{ end -}}
{{- else if eq (len $parts) 1 -}}
{{- $rest = printf "library/%s" $ref -}}
{{- end -}}
{{- $target := (get $mirrors $host) | default ($g.imageRegistry | default "") -}}
{{- if $target -}}
{{- printf "%s/%s" (trimSuffix "/" $target) $rest -}}
{{- else -}}
{{- $ref -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Koinon image reference: koinon.image (list $ .Values.api.image). Built from image.registry (the
upstream; leave it at its default when a mirror is set), the repository and the tag or digest, then
rewritten by koinon.imageRef.
*/}}
{{- define "koinon.image" -}}
{{- $ctx := index . 0 -}}
{{- $img := index . 1 }}
{{- $tag := $img.tag | default $ctx.Values.image.tag | default $ctx.Chart.AppVersion }}
{{- $ref := printf "%s/%s:%s" $ctx.Values.image.registry $img.repository $tag }}
{{- if $img.digest }}{{ $ref = printf "%s@%s" $ref $img.digest }}{{ end }}
{{- include "koinon.imageRef" (list $ctx $ref) }}
{{- end }}

{{/*
Harness image reference: koinon.harnessImage (list $ $f.image). Like koinon.image, but with execution.driver=substrate
the default repository "sandbox" becomes "sandbox-substrate", the harness image with the guest server and the
guest launcher (release job harness-substrate, issue #1087). The substrate driver runs the guest server as the
actor's process, so the plain harness image cannot serve it. Any other repository is used as set. The digest of
the entry must be the digest of the image it names.
*/}}
{{- define "koinon.harnessImage" -}}
{{- $ctx := index . 0 -}}
{{- $img := index . 1 -}}
{{- if and (eq (include "koinon.substrate" $ctx) "true") (eq $img.repository "sandbox") }}
{{- $img = set (deepCopy $img) "repository" "sandbox-substrate" }}
{{- end }}
{{- include "koinon.image" (list $ctx $img) }}
{{- end }}

{{- define "koinon.podSecurityContext" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
fsGroup: 65532
seccompProfile:
  type: RuntimeDefault
{{- end }}

{{- define "koinon.containerSecurityContext" -}}
runAsNonRoot: true
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
privileged: false
capabilities:
  drop: [ALL]
seccompProfile:
  type: RuntimeDefault
{{- end }}

{{/*
The image pull Secret names, as JSON: global.imagePullSecrets, de-duplicated. Each entry is {name: x}
or a plain "x". The top-level imagePullSecrets was removed (issue 1024): setting it fails the render.
*/}}
{{- define "koinon.pullSecretNames" -}}
{{- $names := list -}}
{{- $g := .Values.global | default dict -}}
{{- if hasKey .Values "imagePullSecrets" }}{{- fail "imagePullSecrets was removed: move its entries to global.imagePullSecrets (same format, a list of Secret names or {name: x} maps)" }}{{- end -}}
{{- range ($g.imagePullSecrets | default list) -}}
{{- if kindIs "string" . }}{{ $names = append $names . }}{{ else }}{{ $names = append $names .name }}{{ end -}}
{{- end -}}
{{- $names | uniq | toJson -}}
{{- end }}

{{/* imagePullSecrets: [{name: x}] for a pod spec or the CNPG Cluster; empty when there are none. */}}
{{- define "koinon.imagePullSecrets" -}}
{{- $names := include "koinon.pullSecretNames" . | fromJsonArray -}}
{{- if $names -}}
imagePullSecrets:
{{- range $names }}
  - name: {{ . }}
{{- end }}
{{- end }}
{{- end }}

{{/*
The images OpenShell's supervisor pods may run, as JSON (the sandbox admission policy's pin):
sandboxPolicy.supervisorImages, or when that is empty OpenShell 0.1.7's own default, rewritten by
koinon.imageRef so that a mirror needs no extra setting. An explicit list is used as written.
*/}}
{{- define "koinon.supervisorImages" -}}
{{- if .Values.sandboxPolicy.supervisorImages -}}
{{- .Values.sandboxPolicy.supervisorImages | toJson -}}
{{- else -}}
{{- list (include "koinon.imageRef" (list . "ghcr.io/nvidia/openshell/supervisor:0.1.7")) | toJson -}}
{{- end -}}
{{- end }}

{{/* "true" when sandboxes will pull through a mirror or with credentials: the preflight then checks OpenShell's pull-secret setting. */}}
{{- define "koinon.expectSandboxPullSecrets" -}}
{{- $g := .Values.global | default dict -}}
{{- if or (include "koinon.pullSecretNames" . | fromJsonArray) $g.imageRegistry $g.registryMirrors -}}true{{- end -}}
{{- end }}

{{/* Name of the CNPG Cluster. */}}
{{- define "koinon.pgCluster" -}}
{{- include "koinon.componentName" (list . "pg") }}
{{- end }}

{{/* Postgres role of a component in cnpg mode. */}}
{{- define "koinon.pgRole" -}}
{{- printf "koinon_%s" . }}
{{- end }}

{{/* Secret holding a role's password in cnpg mode: koinon.pgSecret (list $ "api"). */}}
{{- define "koinon.pgSecret" -}}
{{- $ctx := index . 0 -}}
{{- $role := index . 1 -}}
{{- index $ctx.Values.postgres.cnpg.existingSecrets $role | default (include "koinon.componentName" (list $ctx (printf "db-%s" $role))) }}
{{- end }}

{{/*
Database environment for a component: koinon.dbEnv (list $ "api"). Every component reads
DATABASE_URL. In cnpg mode the URI names the component's role, libpq/pgx take the generated
password from PGPASSWORD, and the server certificate is verified against the cluster CA (mounted
by koinon.dbVolume).
*/}}
{{- define "koinon.dbEnv" -}}
{{- $ctx := index . 0 -}}
{{- $component := index . 1 }}
{{- if eq $ctx.Values.postgres.mode "cnpg" -}}
- name: PGPASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "koinon.pgSecret" . }}
      key: password
- name: DATABASE_URL
  value: {{ printf "postgres://%s@%s-rw.%s.svc:5432/%s?sslmode=verify-full&sslrootcert=/etc/koinon/db-ca/ca.crt" (include "koinon.pgRole" $component) (include "koinon.pgCluster" $ctx) $ctx.Release.Namespace $ctx.Values.postgres.cnpg.database | quote }}
{{- else -}}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ required "postgres.external.secretName is required in external mode" $ctx.Values.postgres.external.secretName }}
      key: {{ index $ctx.Values.postgres.external.keys $component }}
{{- end }}
{{- end }}

{{- define "koinon.dbVolumeMount" -}}
{{- if eq .Values.postgres.mode "cnpg" -}}
- name: db-ca
  mountPath: /etc/koinon/db-ca
  readOnly: true
{{- end }}
{{- end }}

{{/* Only the CA certificate is projected, never the CA key. */}}
{{- define "koinon.dbVolume" -}}
{{- if eq .Values.postgres.mode "cnpg" -}}
- name: db-ca
  secret:
    secretName: {{ include "koinon.pgCluster" . }}-ca
    items:
      - key: ca.crt
        path: ca.crt
{{- end }}
{{- end }}

{{/* Koinon's own configuration file, mounted read-only into the reconciler, its only reader. */}}
{{- define "koinon.configVolumeMount" -}}
- name: config
  mountPath: /etc/koinon/config
  readOnly: true
{{- end }}

{{- define "koinon.configVolume" -}}
- name: config
  configMap:
    name: {{ include "koinon.componentName" (list . "config") }}
{{- end }}

{{/*
agentgateway's Service as sandboxes reach it: fully qualified, because OpenShell's policy DNS
resolves names without search domains (koinon-gateway.koinon.svc is NXDOMAIN there).
*/}}
{{- define "koinon.gatewayHost" -}}
{{- printf "%s.%s.svc.%s" (include "koinon.componentName" (list . "gateway")) .Release.Namespace (.Values.clusterDomain | default "cluster.local") -}}
{{- end }}

{{/* The port of the gateway's LLM and MCP listener. */}}
{{- define "koinon.gatewayPort" -}}
{{- if eq .Values.gateway.mode "standalone" }}{{ int .Values.gateway.standalone.port }}{{ else }}{{ int .Values.gateway.kubernetes.port }}{{ end -}}
{{- end }}

{{/* True when koinon-gateway serves the MCP path (gateway.mcp.driver=koinon). Empty otherwise. */}}
{{- define "koinon.mcpKoinon" -}}
{{- if eq .Values.gateway.mcp.driver "koinon" }}true{{ end -}}
{{- end }}

{{/* The model driver. gateway.model.driver when set; empty means koinon with execution.driver=substrate (only
koinon-gateway checks the actor JWT, issue #1489, ADR 0097 amendment of 2026-11-27) and agentgateway otherwise. */}}
{{- define "koinon.modelDriver" -}}
{{- if .Values.gateway.model.driver }}{{ .Values.gateway.model.driver }}
{{- else if eq (include "koinon.substrate" .) "true" }}koinon
{{- else }}agentgateway{{ end -}}
{{- end }}

{{/* True when koinon-gateway serves the model path (see koinon.modelDriver). Empty otherwise. */}}
{{- define "koinon.modelKoinon" -}}
{{- if eq (include "koinon.modelDriver" .) "koinon" }}true{{ end -}}
{{- end }}

{{/* The MCP gateway's Service, fully qualified like koinon.gatewayHost. */}}
{{- define "koinon.mcpGatewayHost" -}}
{{- printf "%s.%s.svc.%s" (include "koinon.componentName" (list . "mcp-gateway")) .Release.Namespace (.Values.clusterDomain | default "cluster.local") -}}
{{- end }}

{{/* True when koinon-gateway serves the egress listener (gateway.mcp.koinon.egress.enabled, #1086 part
C2). It needs gateway.mcp.driver=koinon: only koinon-gateway opens the sealed forge credentials. */}}
{{- define "koinon.egressListener" -}}
{{- if .Values.gateway.mcp.koinon.egress.enabled -}}
{{- if not (include "koinon.mcpKoinon" .) -}}
{{- fail "gateway.mcp.koinon.egress.enabled needs gateway.mcp.driver=koinon: only koinon-gateway holds the key the forge credentials are sealed to" -}}
{{- end -}}
true
{{- end -}}
{{- end }}

{{/* The base URL of the egress listener, as sandboxes and koinon-agentd name it. */}}
{{- define "koinon.egressURL" -}}
{{- printf "http://%s:%d" (include "koinon.mcpGatewayHost" .) (int .Values.gateway.mcp.koinon.egress.port) -}}
{{- end }}

{{/* The Secret with the actor token key (ensure-secrets kind ed25519-key): key, the seed (koinon-agentd only), and public.pem (koinon-gateway only). */}}
{{- define "koinon.actorTokenKeyName" -}}
{{- .Values.gateway.mcp.koinon.egress.actorToken.existingSecret | default (include "koinon.componentName" (list . "actor-token-key")) -}}
{{- end }}

{{/*
Which verifier checks the actor token of the egress listener (ADR 0120, amendment of issue #1386, item 4). Until #1094
removes the OpenShell driver, the execution driver selects it:
- koinon.ed25519ActorToken: openshell. koinon-agentd signs with the Ed25519 seed and writes the interim token file into
  the guest; the gateway verifies with the public key. Exit: #1386 PR B, after #1094, removes it.
- koinon.substrateActorJWT: substrate. atenet-egress adds a token that ateapi mints, outside the guest; the gateway
  verifies it against Substrate's issuer (ate-idp-server). No Ed25519 key exists, and agentd signs nothing.
*/}}
{{- define "koinon.ed25519ActorToken" -}}
{{- if and (include "koinon.egressListener" .) (ne (include "koinon.substrate" .) "true") -}}
true
{{- end -}}
{{- end }}

{{/* Substrate: koinon-gateway checks the actor JWT on the model, MCP and egress listeners (issue #1489). */}}
{{- define "koinon.substrateActorJWT" -}}
{{- if eq (include "koinon.substrate" .) "true" -}}
true
{{- end -}}
{{- end }}

{{/*
The verifier flags of a koinon-gateway pod on Substrate (issue #1489). The audience of each listener is fixed in the
binary (actortoken.AudienceModel, AudienceMCP, AudienceEgress), so the chart names only the issuer and its CA.
*/}}
{{- define "koinon.substrateActorJWTArgs" -}}
{{- if include "koinon.substrateActorJWT" . -}}
- --actor-token-verifier=substrate
- {{ printf "--actor-jwt-issuer=%s" (include "koinon.substrateActorJWTIssuer" .) | quote }}
- --actor-jwt-ca-file={{ include "koinon.substrateActorJWTCADir" . }}/ca.crt
{{- end }}
{{- end }}

{{/*
The ingress peer of a gateway listener on Substrate (ADR 0120 amendment of 2026-11-27, issue #1489): only atenet-egress,
which adds the actor JWT. A pod that skips that hop, a sandbox pod included, gets no answer.
*/}}
{{- define "koinon.atenetEgressPeer" -}}
{{- $sel := .Values.execution.substrate.workerNetworkPolicy.atenetEgressSelector | default dict }}
{{- if not (or $sel.matchLabels $sel.matchExpressions) }}
{{- fail "execution.substrate.workerNetworkPolicy.atenetEgressSelector needs matchLabels or matchExpressions: an empty selector would let every pod of the Substrate namespace reach the gateway listeners" }}
{{- end -}}
- namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: {{ .Values.execution.substrate.namespace | quote }}
  podSelector:
    {{- toYaml $sel | nindent 4 }}
{{- end }}

{{/* The egress rule of a koinon-gateway pod to Substrate's issuer (ate-idp-server), for its key set. */}}
{{- define "koinon.substrateIdPEgress" -}}
{{- if include "koinon.substrateActorJWT" . -}}
{{- $aj := .Values.execution.substrate.actorJwt }}
{{- $idpSel := $aj.idpPodSelector | default dict }}
{{- if not (or $idpSel.matchLabels $idpSel.matchExpressions) }}
{{- fail "execution.substrate.actorJwt.idpPodSelector needs matchLabels or matchExpressions: an empty selector would let the gateway reach every pod in the Substrate namespace" }}
{{- end -}}
# The Substrate verifier (ADR 0120, amendment of issue #1386, item 7): the discovery document and the key set of
# ate-idp-server. The pod port (8443, named https), not the Service port 443: a NetworkPolicy matches the port after
# the Service maps it. No route to ateapi.
- to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: {{ .Values.execution.substrate.namespace | quote }}
      podSelector:
        {{- toYaml $idpSel | nindent 8 }}
  ports:
    - {port: {{ int $aj.idpPort }}, protocol: TCP}
{{- end }}
{{- end }}

{{/* The model gateway's Service, fully qualified like koinon.gatewayHost. */}}
{{- define "koinon.modelGatewayHost" -}}
{{- printf "%s.%s.svc.%s" (include "koinon.componentName" (list . "model-gateway")) .Release.Namespace (.Values.clusterDomain | default "cluster.local") -}}
{{- end }}

{{/* koinon-gateway's model listener as sandboxes and the reconciler name it (gateway.model.driver=koinon). */}}
{{- define "koinon.modelGatewayURL" -}}
{{- printf "http://%s:%d" (include "koinon.modelGatewayHost" .) (int .Values.gateway.model.koinon.port) -}}
{{- end }}

{{/* The one issuer the gateway trusts (execution.substrate.actorJwt.issuer). Empty: Substrate's installer default,
https://idp.<execution.substrate.namespace>.svc (ate-setup actorJWT.issuer; the Service idp of ate-idp-server). */}}
{{- define "koinon.substrateActorJWTIssuer" -}}
{{- $i := .Values.execution.substrate.actorJwt.issuer | default (printf "https://idp.%s.svc" .Values.execution.substrate.namespace) -}}
{{- if not (regexMatch "^https://[^/?#]+$" $i) -}}
{{- fail (printf "execution.substrate.actorJwt.issuer %q must be https://<host> with no path, query or fragment, as Substrate's issuer" $i) -}}
{{- end -}}
{{- $i -}}
{{- end }}

{{- define "koinon.substrateActorJWTCADir" -}}
/etc/koinon/substrate-idp-ca
{{- end }}

{{/* The CA bundle that verifies the issuer's TLS certificate: public certificates only, never a key. */}}
{{- define "koinon.substrateActorJWTCAVolumeMount" -}}
{{- if include "koinon.substrateActorJWT" . -}}
- name: substrate-idp-ca
  mountPath: {{ include "koinon.substrateActorJWTCADir" . }}
  readOnly: true
{{- end }}
{{- end }}

{{/*
Default: the ClusterTrustBundle of Substrate's servicedns signer, which signs the serving certificate of ate-idp-server
(manifests/ate-install/ate-idp-server.yaml, podCertificate servicedns.podcert.ate.dev/identity). The kubelet keeps the
file in step with the bundle, so a CA rotation needs no chart change. execution.substrate.actorJwt.ca.configMap
replaces it, for an issuer that a different CA signs (an issuer outside the cluster).
*/}}
{{- define "koinon.substrateActorJWTCAVolume" -}}
{{- if include "koinon.substrateActorJWT" . -}}
{{- $ca := .Values.execution.substrate.actorJwt.ca }}
- name: substrate-idp-ca
  projected:
    defaultMode: 0444
    sources:
      {{- if $ca.configMap }}
      - configMap:
          name: {{ $ca.configMap | quote }}
          items:
            - key: {{ $ca.configMapKey | quote }}
              path: ca.crt
      {{- else }}
      - clusterTrustBundle:
          signerName: {{ required "execution.substrate.actorJwt.ca.signerName is required without execution.substrate.actorJwt.ca.configMap" $ca.signerName | quote }}
          {{- with $ca.labelSelector }}
          labelSelector:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          path: ca.crt
      {{- end }}
{{- end }}
{{- end }}

{{/* The URL koinon-api sends approved MCP calls to. */}}
{{- define "koinon.gatewayMCPURL" -}}
{{- if include "koinon.mcpKoinon" . -}}
{{- printf "http://%s:%d/mcp" (include "koinon.mcpGatewayHost" .) (int .Values.gateway.mcp.koinon.port) -}}
{{- else -}}
{{- printf "http://%s:%s/mcp" (include "koinon.gatewayHost" .) (include "koinon.gatewayPort" .) -}}
{{- end -}}
{{- end }}

{{/*
The URL koinon-api uses for its live check of an agent's MCP tools: the gateway that serves the agents'
MCP path (koinon.agentMCPURL as koinon-api reaches it). With driver koinon that is koinon-gateway
(koinon.gatewayMCPURL); with agentgateway it is agentgateway's /mcp (ADR 0097 decision 5, #1239).
*/}}
{{- define "koinon.gatewayMCPCheckURL" -}}
{{- include "koinon.gatewayMCPURL" . -}}
{{- end }}

{{/*
The MCP URL an agent's harness gets (koinon-agentd's KOINON_AGENT_MCP_URL). Only with driver koinon:
koinon-gateway's /mcp. With agentgateway the chart sets nothing, and koinon-agentd uses <KOINON_GATEWAY_URL>/mcp
as before (agentd.gatewayURL can name another agentgateway address). The model path (/v1) stays on
agentgateway in both cases.
*/}}
{{- define "koinon.agentMCPURL" -}}
{{- if include "koinon.mcpKoinon" . -}}
{{- include "koinon.gatewayMCPURL" . -}}
{{- end -}}
{{- end }}

{{/*
The Secret that holds the MCP credential files and CA bundles the reconciler opens (mcp-<id>-<hash>,
mcpca-<id>-<hash>). It is separate from the provider keys Secret, so koinon-gateway mounts no LLM key
(ADR 0097 decision 5). Standalone mode only; agentgateway mounts both (it serves MCP with the agentgateway driver,
and with either driver its config keeps the MCP targets whose credential it can open).
*/}}
{{- define "koinon.gatewayMCPKeysName" -}}
{{- include "koinon.componentName" (list . "gateway-mcp-keys") }}
{{- end }}

{{/* Where a gateway pod mounts the MCP credentials Secret. */}}
{{- define "koinon.gatewayMCPKeysDir" -}}/secrets/mcp{{- end }}

{{/*
koinon-gateway's own database login (gateway.mcp.driver=koinon). In cnpg mode the URL file is a
ConfigMap (no password in it: the password is PGPASSWORD, from the generated Secret); in external mode the
file is a key of the external Secret.
*/}}
{{- define "koinon.mcpGatewayDBURLConfigName" -}}
{{- include "koinon.componentName" (list . "mcp-gateway-db") }}
{{- end }}

{{- define "koinon.mcpGatewayDBURLDir" -}}/etc/koinon/mcp-db{{- end }}

{{/*
The labels the agentgateway controller puts on the proxy pods of Koinon's Gateway (kubernetes mode):
the Gateway's name is the label value. NetworkPolicies select the proxy by them.
*/}}
{{- define "koinon.proxySelectorLabels" -}}
gateway.networking.k8s.io/gateway-name: {{ include "koinon.componentName" (list . "gateway") }}
{{- end }}

{{- define "koinon.configEnv" -}}
- name: KOINON_CONFIG
  value: /etc/koinon/config/config.yaml
{{- end }}

{{- define "koinon.commonEnv" -}}
- name: POD_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
{{- /* The container's memory limit in bytes; the binaries set the Go runtime's soft limit to 80% of it (internal/memlimit). */}}
- name: KOINON_MEMORY_LIMIT_BYTES
  valueFrom:
    resourceFieldRef:
      resource: limits.memory
      divisor: "1"
{{- /* Restore-hold (issue #462): all three Deployments start but act on nothing until the value is false again. */}}
{{- if include "koinon.restoreHold" . }}
- name: KOINON_RESTORE_HOLD
  value: "true"
{{- end }}
{{- end }}

{{/*
The URL browsers reach Koinon at: publicUrl, else derived from the ingress, else https:// and the
HTTPRoute's first hostname, else empty (which koinon-api takes as https).
*/}}
{{- define "koinon.publicUrl" -}}
{{- if .Values.publicUrl -}}
{{- trimSuffix "/" .Values.publicUrl -}}
{{- else if .Values.ingress.enabled -}}
{{- printf "%s://%s" (ternary "https" "http" (not (empty .Values.ingress.tlsSecretName))) .Values.ingress.host -}}
{{- else if and .Values.httpRoute.enabled .Values.httpRoute.hostnames -}}
{{- printf "https://%s" (first .Values.httpRoute.hostnames) -}}
{{- end -}}
{{- end }}

{{/* The Secret holding the bootstrap admin's one-time password (key `password`). */}}
{{- define "koinon.bootstrapSecretName" -}}
{{- .Values.bootstrapAdmin.existingSecret | default (include "koinon.componentName" (list . "bootstrap-admin")) -}}
{{- end }}

{{/*
koinon-api's login settings (plan items A1, A2), as KOINON_* environment variables for its flags.
Secrets are mounted as files (koinon.apiSecretVolumes), never put in the environment.
*/}}
{{- define "koinon.apiEnv" -}}
{{- if .Values.api.durable.enabled }}
# The DBOS executor ID is the pod name: unique per replica and never reused, so a replica that dies
# can be told from the one that replaces it, and the survivors recover its workflows (ADR 0085).
- name: KOINON_EXECUTOR_ID
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
{{- end }}
# Approved calls to remote MCP servers go through the gateway (ADR 0057): it holds the credentials.
- name: KOINON_GATEWAY_MCP_URL
  value: {{ include "koinon.gatewayMCPURL" . | quote }}
# The live check of an agent's MCP tools uses the gateway the agents use: koinon-gateway with driver koinon, else agentgateway.
- name: KOINON_GATEWAY_MCP_CHECK_URL
  value: {{ include "koinon.gatewayMCPCheckURL" . | quote }}
# kubernetes mode cannot render a server's own CA or a skipped certificate check; the API refuses them (#278).
- name: KOINON_GATEWAY_MODE
  value: {{ .Values.gateway.mode | quote }}
{{- if include "koinon.mcpKoinon" . }}
# koinon-api seals a stored MCP credential to the gateway's public key once a key is active (ADR 0097 decision
# 12.5 step 0). With this driver koinon-gateway serves every MCP path of the agents and of koinon-api, and
# opens v2. With agentgateway koinon-api seals v1, which agentgateway can read. agentgateway leaves a sealed
# target out of its config; with this driver the reconciler's MCP prober lists tools through koinon-gateway.
- name: KOINON_GATEWAY_MCP_SEAL_V2
  value: "true"
# Sealing starts only when this many pods of every component report that they read the sealed format
# (ADR 0097 decision 12.5 step 1, internal/formatgate); KOINON_GATEWAY_MCP_SEAL_V2 can only close that gate.
- name: KOINON_COMPAT_EXPECT
  value: {{ printf "api=%d,reconciler=%d,gateway=%d" (int .Values.api.replicas) (int .Values.reconciler.replicas) (int .Values.gateway.mcp.koinon.replicas) | quote }}
{{- end }}
# The provider echo check (rc.6) sends one request through the gateway's LLM listener.
- name: KOINON_GATEWAY_URL
  value: {{ printf "http://%s:%s" (include "koinon.gatewayHost" .) (include "koinon.gatewayPort" .) | quote }}
# Every request to the internal OTLP listener needs a bearer token: /v1/logs the gateway's (first directory),
# /v1/traces the one only koinon-agentd holds (second directory); neither admits the other's path.
- name: KOINON_OTLP_TOKEN_DIR
  value: {{ include "koinon.otlpTokenDir" . | quote }}
- name: KOINON_OTLP_TRACES_TOKEN_DIR
  value: {{ include "koinon.otlpTracesTokenDir" . | quote }}
- name: KOINON_KEK_ACTIVE_PERIOD
  value: {{ .Values.secrets.kek.activePeriod | quote }}
{{- if (.Values.restore).acceptImporterDeletions }}
{{- /* ADR 0110 decision 1: accept the declarative removals a restore brought in. NOTES warns while it is set. */}}
- name: KOINON_RESTORE_ACCEPT_IMPORTER_DELETIONS
  value: "true"
{{- end }}
{{- with include "koinon.publicUrl" . }}
- name: KOINON_PUBLIC_URL
  value: {{ . | quote }}
{{- end }}
{{- if .Values.bootstrapAdmin.enabled }}
- name: KOINON_BOOTSTRAP_ADMIN_USERNAME
  value: {{ .Values.bootstrapAdmin.username | quote }}
- name: KOINON_BOOTSTRAP_PASSWORD_FILE
  value: /etc/koinon/bootstrap-admin/password
{{- else }}
- name: KOINON_BOOTSTRAP_LOGIN
  value: "false"
{{- end }}
{{- $o := .Values.oidc }}
{{- if $o.enabled }}
- name: KOINON_OIDC_ISSUER_URL
  value: {{ $o.issuerUrl | quote }}
- name: KOINON_OIDC_CLIENT_ID
  value: {{ $o.clientId | quote }}
{{- if $o.clientSecret.existingSecret }}
- name: KOINON_OIDC_CLIENT_SECRET_FILE
  value: /etc/koinon/oidc/{{ $o.clientSecret.key }}
{{- end }}
{{- with $o.redirectUrl }}
- name: KOINON_OIDC_REDIRECT_URL
  value: {{ . | quote }}
{{- end }}
- name: KOINON_OIDC_SCOPES
  value: {{ join "," $o.scopes | quote }}
- name: KOINON_OIDC_GROUPS_CLAIM
  value: {{ $o.groupsClaim | quote }}
{{- $mappings := list }}
{{- range $group, $role := $o.groupMappings }}
{{- $mappings = append $mappings (printf "%s=%s" $group $role) }}
{{- end }}
- name: KOINON_DEFAULT_ORG_GROUP_MAPPINGS
  value: {{ join "," $mappings | quote }}
- name: KOINON_OIDC_PLATFORM_ADMIN_GROUPS
  value: {{ join "," $o.platformAdminGroups | quote }}
{{- end }}
- name: KOINON_DEFAULT_ORG_IDENTITY_MODE
  value: {{ $o.defaultOrgIdentityMode | quote }}
- name: KOINON_SESSION_IDLE_TIMEOUT
  value: {{ .Values.session.idleTimeout | quote }}
- name: KOINON_SESSION_MAX_LIFETIME
  value: {{ .Values.session.maxLifetime | quote }}
{{- end }}

{{- define "koinon.apiSecretVolumeMounts" -}}
{{ include "koinon.otlpTokenVolumeMount" . }}
{{ include "koinon.otlpTracesTokenVolumeMount" . }}
{{- if .Values.bootstrapAdmin.enabled }}
- name: bootstrap-admin
  mountPath: /etc/koinon/bootstrap-admin
  readOnly: true
{{- end }}
{{- if and .Values.oidc.enabled .Values.oidc.clientSecret.existingSecret }}
- name: oidc-client-secret
  mountPath: /etc/koinon/oidc
  readOnly: true
{{- end }}
{{- end }}

{{- define "koinon.apiSecretVolumes" -}}
{{ include "koinon.otlpTokenVolume" . }}
{{ include "koinon.otlpTracesTokenVolume" . }}
{{- if .Values.bootstrapAdmin.enabled }}
- name: bootstrap-admin
  secret:
    secretName: {{ include "koinon.bootstrapSecretName" . }}
    items:
      - key: password
        path: password
{{- end }}
{{- if and .Values.oidc.enabled .Values.oidc.clientSecret.existingSecret }}
- name: oidc-client-secret
  secret:
    secretName: {{ .Values.oidc.clientSecret.existingSecret }}
    items:
      - key: {{ .Values.oidc.clientSecret.key }}
        path: {{ .Values.oidc.clientSecret.key }}
{{- end }}
{{- end }}

{{/* GitHub REST API base URL, for the components that call GitHub (api and reconciler). */}}
{{- define "koinon.githubEnv" -}}
- name: KOINON_GITHUB_API_URL
  value: {{ .Values.github.apiUrl | default "https://api.github.com" | quote }}
{{- end }}

{{/* The cluster's DNS domain. koinon-api and koinon-reconciler check a connector's base URL against the
resolver of a sandbox guest, which expands a short name with search domains built from it (#1054,
internal/catalog/urlpolicy.go). The same value builds the full Service names elsewhere in this chart. */}}
{{- define "koinon.clusterDomainEnv" -}}
- name: KOINON_CLUSTER_DOMAIN
  value: {{ .Values.clusterDomain | default "cluster.local" | quote }}
{{- end }}

{{/*
The OpenShell gateway's host:port. The reconciler and agentd refuse to start without it, so the
chart refuses to render without it (the install would otherwise end in crash-looping pods).
*/}}
{{- define "koinon.openshellAddress" -}}
{{- required "openshell.address is required: set it to the OpenShell gateway's host:port, for example openshell.openshell.svc:8080" .Values.openshell.address -}}
{{- end }}

{{/*
koinon.substrate.apiServerEgress is the one egress rule that lets a fenced control pod reach the Kubernetes API
server (ADR 0120 section 2, amendment 10, issue 1172). It names TCP ports and NO destination, so the peer is any
address. NetworkPolicy has no CNI-neutral peer for the API server: an ipBlock for the Service address or the
endpoint address matches on one CNI and times out on another (Cilium with its default policy-cidr-match-mode
never matches an ipBlock against a node address). An empty port list would open every port, so it fails the
render. Only the pods that call the API server use this rule; worker pods never do.
*/}}
{{- define "koinon.substrate.apiServerEgress" -}}
{{- $ports := .Values.execution.substrate.workerNetworkPolicy.apiServerPorts -}}
{{- if not $ports }}{{- fail "execution.substrate.workerNetworkPolicy.apiServerPorts is empty: the policy would cut the Substrate control plane off from the Kubernetes API server, and an empty port list in a NetworkPolicy would open every port. List the TCP ports of the API server (443 and 6443 by default), or set workerNetworkPolicy.enabled=false and fence the namespaces yourself." }}{{- end -}}
- ports:
{{- range $ports }}
    - {port: {{ . }}, protocol: TCP}
{{- end }}
{{- end }}

{{- define "koinon.substrate" -}}
{{- eq (.Values.execution.driver | default "openshell") "substrate" -}}
{{- end }}

{{/*
koinon.substrate.workerPoolImage: the worker image of the WorkerPool that the chart makes, after the registry mirrors
(koinon.imageRef), or "" when the chart makes no pool. An empty workerPool.workerImage takes the worker image of the
pinned Substrate release for the pool's class: <repository>/ateworker-<class>:<version>. The pin is the one of
internal/preflight/substratepin.go (SubstratePinnedImageRepo, SubstratePinnedVersion); test/chartsmoke fails when the two
differ. Substrate v1.0.1 publishes tags only (see workerPolicy.digestExemptIdentities); pin a digest in workerImage.
*/}}
{{- define "koinon.substrate.pinnedImageRepo" -}}ghcr.io/agent-substrate/substrate{{- end }}
{{- define "koinon.substrate.pinnedVersion" -}}v1.0.1{{- end }}
{{/*
koinon.substrate.workerMemoryNeed gives the memory (bytes, float) that one worker pod's limit must have to
place an actor with a memory limit of .mem bytes, for sandbox class .class. The worker offers its pod
limits.memory less a reserve as capacity, and the scheduler books each actor at its limit plus the
per-actor overhead of the class. Substrate v1.0.1 (20778a6d):
- reserve 64Mi in both classes: 32Mi + 2 x 16Mi (cmd/ateworker-gvisor/memory.go:27-35,
  cmd/ateworker-microvm/run.go:152 with checkpoint.go:336-337)
- overhead: gvisor none (cmd/ateworker-gvisor/main.go:315-328); microvm 4Mi + 8Mi per GiB of the limit
  (cmd/ateworker-microvm/run.go:147, booked by cmd/ateapi/internal/scheduling/scheduling.go:278-293)
*/}}
{{- define "koinon.substrate.workerMemoryNeed" -}}
{{- $mi := 1048576.0 -}}
{{- $need := addf .mem (mulf 64.0 $mi) -}}
{{- if eq .class "microvm" -}}
{{- $need = addf $need (mulf 4.0 $mi) (mulf (divf .mem 1073741824.0) (mulf 8.0 $mi)) -}}
{{- end -}}
{{- $need -}}
{{- end }}

{{- define "koinon.substrate.workerPoolImage" -}}
{{- $p := .Values.execution.substrate.workerPool -}}
{{- if and (eq (include "koinon.substrate" .) "true") $p.enabled -}}
{{- $img := $p.workerImage -}}
{{- if not $img -}}
{{- $img = printf "%s/ateworker-%s:%s" (include "koinon.substrate.pinnedImageRepo" .) $p.sandboxClass (include "koinon.substrate.pinnedVersion" .) -}}
{{- end -}}
{{- include "koinon.imageRef" (list . $img) -}}
{{- end -}}
{{- end }}

{{/*
koinon.narrowCIDR fails the render when a CIDR of workerNetworkPolicy.imageSource.cidrs is wider than one image source
needs (issue #1176). The rule reads the prefix length, so every spelling of a range is caught (0.0.0.0/0, 0.0.0.0/1,
1.0.0.0/8, ::/0, 0::/0, 2000::/3):
- IPv4: /16 or longer. /8 to /15 only inside private space (10.0.0.0/8, 172.16.0.0/12). Shorter than /8 never.
- IPv6: /48 or longer. /7 to /47 only inside unique local space (fc00::/7). Shorter than /7 never.
- No IPv6 range inside ::/8: it holds loopback, IPv4-mapped (::ffff:0:0/96) and NAT64 (64:ff9b::/96,
  64:ff9b:1::/48) space, which is IPv4 space in another spelling.
Argument: the CIDR string.
*/}}
{{- define "koinon.narrowCIDR" -}}
{{- $c := toString . -}}
{{- $what := "execution.substrate.workerNetworkPolicy.imageSource.cidrs" -}}
{{- if regexMatch "^[0-9]{1,3}\\.[0-9]{1,3}\\.[0-9]{1,3}\\.[0-9]{1,3}/[0-9]{1,2}$" $c -}}
{{- $parts := splitList "/" $c -}}
{{- $o := splitList "." (first $parts) -}}
{{- $bits := atoi (last $parts) -}}
{{- $a := atoi (index $o 0) -}}
{{- $b := atoi (index $o 1) -}}
{{- range $o }}{{- if gt (atoi .) 255 }}{{- fail (printf "%s: %s is not an IPv4 CIDR" $what $c) }}{{- end }}{{- end -}}
{{- if gt $bits 32 }}{{- fail (printf "%s: %s is not an IPv4 CIDR" $what $c) }}{{- end -}}
{{- $private := or (and (eq $a 10) (ge $bits 8)) (and (eq $a 172) (ge $b 16) (le $b 31) (ge $bits 12)) -}}
{{- if and (lt $bits 16) (not $private) }}{{- fail (printf "%s: %s is wider than one image source needs: use /16 or longer (/8 to /15 only inside 10.0.0.0/8 or 172.16.0.0/12)" $what $c) }}{{- end -}}
{{- else if regexMatch "^[0-9a-fA-F:]+/[0-9]{1,3}$" $c -}}
{{- $parts := splitList "/" $c -}}
{{- $addr := lower (first $parts) -}}
{{- $bits := atoi (last $parts) -}}
{{- if gt $bits 128 }}{{- fail (printf "%s: %s is not an IPv6 CIDR" $what $c) }}{{- end -}}
{{- if contains "ffff:" $addr }}{{- if regexMatch "^(0{0,4}:){0,5}:?ffff:" $addr }}{{- fail (printf "%s: %s is an IPv4-mapped range: name the IPv4 CIDR" $what $c) }}{{- end }}{{- end -}}
{{- $first := first (splitList ":" $addr) -}}
{{- if regexMatch "^0{0,2}[0-9a-f]{0,2}$" $first }}{{- fail (printf "%s: %s is inside ::/8 (loopback, IPv4-mapped or NAT64 64:ff9b::/96 space): name the IPv4 CIDR or a global IPv6 range" $what $c) }}{{- end -}}
{{- $ula := and (ge $bits 7) (regexMatch "^f[cd][0-9a-f]{2}$" $first) -}}
{{- if and (lt $bits 48) (not $ula) }}{{- fail (printf "%s: %s is wider than one image source needs: use /48 or longer (/7 to /47 only inside fc00::/7)" $what $c) }}{{- end -}}
{{- else -}}
{{- fail (printf "%s: %s is not a CIDR (address/prefix)" $what $c) -}}
{{- end -}}
{{- end }}

{{/*
The in-cluster image cache of the Substrate driver (issue #1170): "true" when the driver is substrate and
execution.substrate.imageCache.enabled. The cache is a Zot pull-through registry in the release namespace; it is the one
image source of the node agent, and the driver rewrites the registry host of every actor image to it.
*/}}
{{- define "koinon.imageCache" -}}
{{- if and (eq (include "koinon.substrate" .) "true") .Values.execution.substrate.imageCache.enabled -}}true{{- end -}}
{{- end }}

{{- define "koinon.imageCache.name" -}}
{{- include "koinon.componentName" (list . "imagecache") -}}
{{- end }}

{{/* The TLS Secret of the cache: the operator's, or the one the ensure-secrets hook Job makes. */}}
{{- define "koinon.imageCache.tlsSecret" -}}
{{- .Values.execution.substrate.imageCache.tls.existingSecret | default (include "koinon.componentName" (list . "imagecache-tls")) -}}
{{- end }}

{{/* The address the node agent dials: <service>.<namespace>.svc:<port>. Part of the template hash of every actor. */}}
{{- define "koinon.imageCache.host" -}}
{{- printf "%s.%s.svc:%d" (include "koinon.imageCache.name" .) .Release.Namespace (int .Values.execution.substrate.imageCache.port) -}}
{{- end }}

{{/* The DNS names of the certificate of the cache, as JSON. */}}
{{- define "koinon.imageCache.dnsNames" -}}
{{- $n := include "koinon.imageCache.name" . -}}
{{- $svc := printf "%s.%s" $n .Release.Namespace -}}
{{- list $n $svc (printf "%s.svc" $svc) (printf "%s.svc.%s" $svc .Values.execution.substrate.imageCache.clusterDomain) | toJson -}}
{{- end }}

{{/*
The "host=target" entries that rewrite the registry host of an actor image, as JSON (the driver's
--substrate-image-mirrors). With the cache: each upstream host maps to <cache host>/<host with ':' written '_'> (a port in a repository path is not a valid image reference). Otherwise: the operator's
execution.substrate.imageMirrors. The two together fail the render, because the node agent has one image source.
*/}}
{{- define "koinon.imageMirrorEntries" -}}
{{- $s := .Values.execution.substrate -}}
{{- $entries := list -}}
{{- if include "koinon.imageCache" . -}}
{{- if or $s.imageMirrors $s.imageRegistry -}}{{- fail "execution.substrate.imageCache.enabled is the one image source: do not set imageMirrors or imageRegistry with it. For an operator mirror set imageCache.enabled=false" -}}{{- end -}}
{{- if not $s.imageCache.upstreams -}}{{- fail "execution.substrate.imageCache.upstreams is empty: the cache would sync nothing" -}}{{- end -}}
{{- range $s.imageCache.upstreams -}}
{{- if and (hasPrefix "http://" .url) (not .allowHTTP) -}}{{- fail (printf "execution.substrate.imageCache.upstreams: %s is a plain http URL; set allowHTTP: true on that entry to accept it (a private registry), and add its range to imageCache.egress.extra" .host) -}}{{- end -}}
{{- $entries = append $entries (printf "%s=%s/%s" (lower .host) (include "koinon.imageCache.host" $) (lower .host | replace ":" "_")) -}}
{{- end -}}
{{- else -}}
{{- range $host := keys ($s.imageMirrors | default dict) | sortAlpha -}}
{{- $entries = append $entries (printf "%s=%s" (lower $host) (get $s.imageMirrors $host)) -}}
{{- end -}}
{{- end -}}
{{- $entries | toJson -}}
{{- end }}

{{/* The namespaces whose pods may reach the gateway's model port: OpenShell's sandbox namespaces, or with the substrate driver the namespace atenet runs in. */}}
{{- define "koinon.sandboxClientNamespaceSelector" -}}
{{- if eq (include "koinon.substrate" .) "true" -}}
{{- toYaml .Values.execution.substrate.gatewayClientNamespaceSelector -}}
{{- else -}}
{{- toYaml .Values.sandboxPolicy.namespaceSelector -}}
{{- end -}}
{{- end }}

{{- define "koinon.substrateTokenDir" -}}
/var/run/koinon/substrate
{{- end }}

{{/*
The Secret with the key that signs the guest API tokens (issue #1309): key "key", the hex seed of an
Ed25519 key. Only koinon-reconciler and koinon-agentd mount it. Never mounted into a sandbox: the
actor template carries only the public key.
*/}}
{{- define "koinon.substrateGuestKeyName" -}}
{{- .Values.execution.substrate.guestKey.existingSecret | default (include "koinon.componentName" (list . "substrate-guest-key")) }}
{{- end }}

{{- define "koinon.substrateGuestKeyFile" -}}
/secrets/substrate-guest-key/key
{{- end }}

{{/* ateapi's token and CA, and the guest API key, for the components that call Agent Substrate (execution.driver=substrate). */}}
{{- define "koinon.substrateVolumeMount" -}}
{{- if eq (include "koinon.substrate" .) "true" -}}
- name: substrate-token
  mountPath: {{ include "koinon.substrateTokenDir" . }}
  readOnly: true
- name: substrate-guest-key
  mountPath: /secrets/substrate-guest-key
  readOnly: true
{{- if .Values.execution.substrate.caSecret }}
- name: substrate-ca
  mountPath: /etc/koinon/substrate-ca
  readOnly: true
{{- end }}
{{- end }}
{{- end }}

{{/*
The provisioner (ADR 0128): "true" with execution.driver=substrate and execution.substrate.provisioner.enabled. Its
ServiceAccount is the only Koinon principal that holds a global role in Substrate.
*/}}
{{- define "koinon.provisioner" -}}
{{- if and (eq (include "koinon.substrate" .) "true") .Values.execution.substrate.provisioner.enabled -}}true{{- end -}}
{{- end }}

{{- define "koinon.provisionerTokenDir" -}}
/var/run/koinon/provisioner
{{- end }}

{{/* The reconciler's token for the provisioner (audience execution.substrate.provisioner.tokenAudience). Only the reconciler mounts it. */}}
{{- define "koinon.provisionerVolumeMount" -}}
{{- if include "koinon.provisioner" . -}}
- name: provisioner-token
  mountPath: {{ include "koinon.provisionerTokenDir" . }}
  readOnly: true
{{- end }}
{{- end }}

{{- define "koinon.provisionerVolume" -}}
{{- if include "koinon.provisioner" . -}}
- name: provisioner-token
  projected:
    sources:
      - serviceAccountToken:
          audience: {{ .Values.execution.substrate.provisioner.tokenAudience | quote }}
          expirationSeconds: 3600
          path: token
{{- end }}
{{- end }}

{{/*
koinon-agentd's identity for the atenet router (issue #1446): the router serves TLS only and lets a client in by the
SPIFFE ID of its certificate. A projected volume holds the pod certificate (signer podidentity.podcert.ate.dev/identity,
which issues spiffe://cluster.local/ns/<namespace>/sa/<ServiceAccount>) and the CA of the router's certificate (the
ClusterTrustBundle of the servicedns signer, as in Substrate v1.0.1 cmd/atenet/internal/router/README.md "Client in the
cluster, pod identity CA"). The kubelet rotates both files in place and the driver reads them again. Only agentd mounts
it: the allow-list has the agentd ServiceAccount and no other Koinon pod, because any allowed ID can reach any actor.
*/}}
{{- define "koinon.substrateRouterDir" -}}
/etc/koinon/substrate-router
{{- end }}

{{- define "koinon.substrateRouterVolumeMount" -}}
{{- if eq (include "koinon.substrate" .) "true" -}}
- name: substrate-router
  mountPath: {{ include "koinon.substrateRouterDir" . }}
  readOnly: true
{{- end }}
{{- end }}

{{- define "koinon.substrateRouterVolume" -}}
{{- if eq (include "koinon.substrate" .) "true" -}}
- name: substrate-router
  projected:
    sources:
      - podCertificate:
          signerName: podidentity.podcert.ate.dev/identity
          keyType: ECDSAP256
          credentialBundlePath: credential-bundle.pem
      - clusterTrustBundle:
          signerName: servicedns.podcert.ate.dev/identity
          labelSelector:
            matchLabels:
              podcert.ate.dev/canarying: live
          path: router-ca.pem
{{- end }}
{{- end }}

{{- define "koinon.substrateVolume" -}}
{{- if eq (include "koinon.substrate" .) "true" -}}
- name: substrate-token
  projected:
    sources:
      - serviceAccountToken:
          audience: {{ required "execution.substrate.tokenAudience is required with execution.driver=substrate" .Values.execution.substrate.tokenAudience | quote }}
          expirationSeconds: 3600
          path: token
- name: substrate-guest-key
  secret:
    secretName: {{ include "koinon.substrateGuestKeyName" . }}
    defaultMode: 0440
    items:
      - key: key
        path: key
{{- if .Values.execution.substrate.caSecret }}
- name: substrate-ca
  secret:
    secretName: {{ .Values.execution.substrate.caSecret }}
{{- end }}
{{- end }}
{{- end }}

{{/* OpenShell client certificate for components that call the OpenShell gateway. */}}
{{- define "koinon.openshellVolumeMount" -}}
{{- if .Values.openshell.clientTLSSecret -}}
- name: openshell-tls
  mountPath: /etc/koinon/openshell-tls
  readOnly: true
{{- end }}
{{- end }}

{{- define "koinon.openshellVolume" -}}
{{- if .Values.openshell.clientTLSSecret -}}
- name: openshell-tls
  secret:
    secretName: {{ .Values.openshell.clientTLSSecret }}
{{- end }}
{{- end }}

{{/*
Labels of a Secret the ensure-secrets Job creates: koinon.generatedLabels (list $ "api"). The
release does not own these Secrets, so they carry no chart version or managed-by label that would
go stale, and no app.kubernetes.io/instance label: Argo CD's label tracking would count them as
app resources that are missing from Git, and auto-prune would delete them (the next sync would then
generate new passwords). koinon.dev/release names the release instead.
*/}}
{{- define "koinon.generatedLabels" -}}
{{- $ctx := index . 0 -}}
app.kubernetes.io/name: {{ include "koinon.name" $ctx }}
app.kubernetes.io/component: {{ index . 1 }}
app.kubernetes.io/part-of: koinon
koinon.dev/release: {{ $ctx.Release.Name }}
koinon.dev/generated: password
{{- end }}

{{/*
The Secrets the ensure-secrets hook Job creates when they are missing, as JSON (empty list: no Job):
koinon.ensureSecrets. Roles and the bootstrap admin that come from an existing Secret are left out.
*/}}
{{- define "koinon.ensureSecrets" -}}
{{- $specs := list -}}
{{- if eq .Values.postgres.mode "cnpg" -}}
{{- $roles := list "migrate" "api" "reconciler" "agentd" -}}
{{- if include "koinon.mcpKoinon" . -}}{{- $roles = append $roles "mcpgateway" -}}{{- end -}}
{{- if include "koinon.gwKey" . -}}{{- $roles = concat $roles (list "gwkeyadmin" "reseal") -}}{{- end -}}
{{- range $role := $roles -}}
{{- if not (index $.Values.postgres.cnpg.existingSecrets $role) -}}
{{- $labels := fromYaml (include "koinon.generatedLabels" (list $ "postgres")) | merge (dict "cnpg.io/reload" "true") -}}
{{- $specs = append $specs (dict "name" (include "koinon.pgSecret" (list $ $role)) "type" "kubernetes.io/basic-auth" "username" (include "koinon.pgRole" $role) "labels" $labels) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if and .Values.bootstrapAdmin.enabled (not .Values.bootstrapAdmin.existingSecret) -}}
{{- $specs = append $specs (dict "name" (include "koinon.bootstrapSecretName" .) "type" "Opaque" "username" .Values.bootstrapAdmin.username "labels" (fromYaml (include "koinon.generatedLabels" (list . "api")))) -}}
{{- end -}}
{{- if and (eq .Values.gateway.mode "standalone") (not .Values.gateway.standalone.sessionKey.existingSecret) -}}
{{- $specs = append $specs (dict "name" (include "koinon.gatewaySessionKeyName" .) "type" "Opaque" "kind" "aes256-key" "labels" (fromYaml (include "koinon.generatedLabels" (list . "gateway")))) -}}
{{- end -}}
{{- if not .Values.api.otlpToken.existingSecret -}}
{{- $specs = append $specs (dict "name" (include "koinon.otlpTokenName" .) "type" "Opaque" "kind" "bearer-token" "labels" (fromYaml (include "koinon.generatedLabels" (list . "api")))) -}}
{{- end -}}
{{- if not .Values.api.otlpTracesToken.existingSecret -}}
{{- $specs = append $specs (dict "name" (include "koinon.otlpTracesTokenName" .) "type" "Opaque" "kind" "bearer-token" "labels" (fromYaml (include "koinon.generatedLabels" (list . "api")))) -}}
{{- end -}}
{{- if and (include "koinon.imageCache" .) (not .Values.execution.substrate.imageCache.tls.existingSecret) -}}
{{- $specs = append $specs (dict "name" (include "koinon.imageCache.tlsSecret" .) "type" "kubernetes.io/tls" "kind" "tls" "dnsNames" (include "koinon.imageCache.dnsNames" . | fromJsonArray) "labels" (fromYaml (include "koinon.generatedLabels" (list . "imagecache")))) -}}
{{- end -}}
{{- if and (eq (include "koinon.substrate" .) "true") (not .Values.execution.substrate.guestKey.existingSecret) -}}
{{- $specs = append $specs (dict "name" (include "koinon.substrateGuestKeyName" .) "type" "Opaque" "kind" "ed25519-key" "labels" (fromYaml (include "koinon.generatedLabels" (list . "reconciler")))) -}}
{{- end -}}
{{- if not .Values.installMark.existingSecret -}}
{{- $specs = append $specs (dict "name" (include "koinon.installMarkName" .) "type" "Opaque" "kind" "bearer-token" "labels" (fromYaml (include "koinon.generatedLabels" (list . "reconciler")))) -}}
{{- end -}}
{{- if and (include "koinon.ed25519ActorToken" .) (not .Values.gateway.mcp.koinon.egress.actorToken.existingSecret) -}}
{{- $specs = append $specs (dict "name" (include "koinon.actorTokenKeyName" .) "type" "Opaque" "kind" "ed25519-key" "labels" (fromYaml (include "koinon.generatedLabels" (list . "agentd")))) -}}
{{- end -}}
{{- $specs | toJson -}}
{{- end }}

{{/*
The Secret with this install's mark (issue 461): a random value, key "token", that the hook Job generates once
and never changes. koinon-reconciler and koinon-agentd label every OpenShell object they make with a hash of it
and refuse an object that carries another install's. It is deliberately not in Postgres: a restored database
next to the live install, on the same gateway, must not share it. It is not a credential (only its hash leaves
the pod), but it is a Secret so that it is not rendered into a manifest or a ConfigMap.
*/}}
{{- define "koinon.installMarkName" -}}
{{- .Values.installMark.existingSecret | default (include "koinon.componentName" (list . "install-mark")) }}
{{- end }}

{{- define "koinon.installMarkEnv" -}}
- name: KOINON_INSTALL_MARK
  valueFrom:
    secretKeyRef:
      name: {{ include "koinon.installMarkName" . }}
      key: token
{{- end }}

{{/*
The Secret with the bearer token of koinon-api's internal OTLP listener for /v1/logs (ADR 0074, old issue 52):
key "token", plus "previous" during a rotation. koinon-api mounts it; the standalone gateway mounts it too, and
in kubernetes mode the agentgateway controller reads it by name. koinon-agentd does NOT mount it (issue 55):
agentd's token for /v1/traces is the separate Secret below, so neither token admits the other's path.
Never mounted into a sandbox.
*/}}
{{- define "koinon.otlpTokenName" -}}
{{- .Values.api.otlpToken.existingSecret | default (include "koinon.componentName" (list . "otlp-token")) }}
{{- end }}

{{/* Where the OTLP token Secret is mounted (a directory, not subPath, so a rotation reaches the pod). */}}
{{- define "koinon.otlpTokenDir" -}}/secrets/otlp{{- end }}

{{- define "koinon.otlpTokenVolumeMount" -}}
- name: otlp-token
  mountPath: {{ include "koinon.otlpTokenDir" . }}
  readOnly: true
{{- end }}

{{- define "koinon.otlpTokenVolume" -}}
- name: otlp-token
  secret:
    secretName: {{ include "koinon.otlpTokenName" . }}
    # The keys are the files: token, and previous while a rotation is under way.
    defaultMode: 0440
{{- end }}

{{/*
The Secret with the bearer token for /v1/traces on the same listener (issue 55): key "token", plus "previous"
during a rotation, the same design as the one above. Only koinon-agentd (the sender) and koinon-api (the checker)
mount it; the gateway and the sandboxes never do, so a holder of the gateway's token cannot post spans into a run.
*/}}
{{- define "koinon.otlpTracesTokenName" -}}
{{- if and .Values.api.otlpToken.existingSecret (eq .Values.api.otlpToken.existingSecret .Values.api.otlpTracesToken.existingSecret) }}
{{- fail "api.otlpToken.existingSecret and api.otlpTracesToken.existingSecret name the same Secret: /v1/traces needs a token of its own (issue 55)" }}
{{- end }}
{{- .Values.api.otlpTracesToken.existingSecret | default (include "koinon.componentName" (list . "otlp-traces-token")) }}
{{- end }}

{{- define "koinon.otlpTracesTokenDir" -}}/secrets/otlp-traces{{- end }}

{{- define "koinon.otlpTracesTokenVolumeMount" -}}
- name: otlp-traces-token
  mountPath: {{ include "koinon.otlpTracesTokenDir" . }}
  readOnly: true
{{- end }}

{{- define "koinon.otlpTracesTokenVolume" -}}
- name: otlp-traces-token
  secret:
    secretName: {{ include "koinon.otlpTracesTokenName" . }}
    defaultMode: 0440
{{- end }}

{{/* The Secret with the standalone gateway's MCP session key (agentgateway SESSION_KEY, key "key"). */}}
{{- define "koinon.gatewaySessionKeyName" -}}
{{- .Values.gateway.standalone.sessionKey.existingSecret | default (include "koinon.componentName" (list . "gateway-session-key")) }}
{{- end }}

{{/*
Hook annotations of the preflight objects: koinon.preflightHook (list $ "<weight>"). They run
before every other hook; a failed run is kept for inspection and replaced on the next attempt.
*/}}
{{- define "koinon.preflightHook" -}}
helm.sh/hook: pre-install,pre-upgrade
helm.sh/hook-weight: {{ index . 1 | quote }}
helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded
{{- end }}

{{/* Hook annotations of the ensure-secrets objects: koinon.ensureSecretsHook (list $ "<weight>"). */}}
{{- define "koinon.ensureSecretsHook" -}}
helm.sh/hook: pre-install,pre-upgrade
helm.sh/hook-weight: {{ index . 1 | quote }}
helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded
{{- end }}

{{- define "koinon.gatewayConfigName" -}}
{{- include "koinon.componentName" (list . "gateway-config") }}
{{- end }}

{{/* "true" when the install is held (restoreHold, ADR 0107), else empty. The one test every held-only render uses. */}}
{{- define "koinon.restoreHold" -}}
{{- if eq (toString .Values.restoreHold) "true" }}true{{- end }}
{{- end }}

{{/* The deny-all gateway config a held install mounts instead of the reconciler's (restoreHold, issue #530). */}}
{{/* The suffix is appended after the base is cut, so the 63-character limit cannot drop it and the name always differs from koinon.gatewayConfigName. */}}
{{- define "koinon.gatewayHeldConfigName" -}}
{{- $suffix := "-gateway-config-held" -}}
{{- printf "%s%s" (include "koinon.fullname" . | trunc (int (sub 63 (len $suffix))) | trimSuffix "-") $suffix }}
{{- end }}

{{/* The Secret with the key-encryption keys (KEKs) for provider keys and connection secrets (ADR 0056). */}}
{{- define "koinon.kekSecretName" -}}
{{- .Values.secrets.kek.existingSecret | default (include "koinon.componentName" (list . "kek")) }}
{{- end }}

{{/* The one Secret holding the opened provider keys, mounted into the gateway only (plan item PV1). */}}
{{- define "koinon.gatewayProvidersName" -}}
{{- include "koinon.componentName" (list . "gateway-providers") }}
{{- end }}

{{/*
The KEKs. koinon-api reads them through the Kubernetes API and creates the Secret on its first start
(the chart renders no key material: every render would make new keys, and a new KEK loses every stored
secret). koinon-reconciler mounts them, optionally, because the Secret may not exist before koinon-api
has started.
*/}}
{{- define "koinon.kekVolumeMount" -}}
- name: kek
  mountPath: /secrets/kek
  readOnly: true
{{- end }}

{{- define "koinon.kekVolume" -}}
- name: kek
  secret:
    secretName: {{ include "koinon.kekSecretName" . }}
    defaultMode: 0440
    optional: true
{{- end }}

{{/* Platform-level allowlist of internal hosts that providers and MCP servers may name (SSRF policy,
internal/catalog/urlpolicy.go). providers.allowedInternalHosts is the deprecated name of
egress.allowedInternalHosts; both are joined. */}}
{{- define "koinon.providerEnv" -}}
{{- $hosts := concat (.Values.egress.allowedInternalHosts | default list) (.Values.providers.allowedInternalHosts | default list) | uniq }}
{{- with $hosts }}
- name: KOINON_EGRESS_ALLOWED_HOSTS
  value: {{ join "," . | quote }}
{{- end }}
{{- end }}

{{- define "koinon.kekEnv" -}}
{{- if eq .component "reconciler" }}
- name: KOINON_KEK_DIR
  value: /secrets/kek
{{- else }}
- name: KOINON_KEK_SECRET
  value: {{ include "koinon.kekSecretName" .root | quote }}
- name: KOINON_KEK_CREATE
  value: {{ if or .root.Values.secrets.kek.existingSecret (include "koinon.kms" .root) }}"false"{{ else }}"true"{{ end }}
{{- end }}
{{- include "koinon.kmsEnv" (dict "root" .root "role" .component) }}
{{- end }}

{{/*
The KMS that wraps the KEKs (values secrets.kek.kms, ADR 0085 decision 12): "openbao" or "awskms", or empty for
the local Secret-held KEKs. With a KMS, koinon-api never creates the KEK Secret (KOINON_KEK_CREATE
false): the key-admin Job does (key-admin.yaml), since only it may encrypt.
*/}}
{{- define "koinon.kms" -}}
{{- $b := .Values.secrets.kek.kms.backend | default "local" }}
{{- if ne $b "local" }}{{ $b }}{{ end }}
{{- end }}

{{/*
The KMS settings of one identity (dict root, role: api, reconciler or keyAdmin), as the KOINON_ forms
of the --kek-backend, --openbao-* and --awskms-* flags (internal/secretstore/wrapping). Each identity
signs in with its own service account token as its own role (OpenBao) or IAM role (AWS); nothing
here is a credential.
*/}}
{{- define "koinon.kmsEnv" -}}
{{- $kms := .root.Values.secrets.kek.kms }}
{{- if eq (include "koinon.kms" .root) "openbao" }}
{{- $o := $kms.openbao }}
- name: KOINON_KEK_BACKEND
  value: openbao
- name: KOINON_OPENBAO_ADDRESS
  value: {{ $o.address | quote }}
- name: KOINON_OPENBAO_TRANSIT_MOUNT
  value: {{ $o.transitMount | default "transit" | quote }}
- name: KOINON_OPENBAO_KEY
  value: {{ $o.key | quote }}
- name: KOINON_OPENBAO_AUTH_MOUNT
  value: {{ $o.authMount | default "kubernetes" | quote }}
- name: KOINON_OPENBAO_ROLE
  value: {{ index $o.roles .role | quote }}
{{- with $o.namespace }}
- name: KOINON_OPENBAO_NAMESPACE
  value: {{ . | quote }}
{{- end }}
{{- with $o.timeout }}
- name: KOINON_OPENBAO_TIMEOUT
  value: {{ . | quote }}
{{- end }}
{{- if $o.caConfigMap }}
- name: KOINON_OPENBAO_CA_FILE
  value: /etc/koinon/openbao-ca/ca.crt
{{- end }}
{{- else if eq (include "koinon.kms" .root) "awskms" }}
{{- $a := $kms.awskms }}
{{- /* No credential and no AWS_* variable: IRSA's webhook or the EKS Pod Identity Agent gives the
pod its identity (AWS_ROLE_ARN and a token file), and the adapter never reads static keys. */}}
- name: KOINON_KEK_BACKEND
  value: awskms
- name: KOINON_AWSKMS_REGION
  value: {{ $a.region | quote }}
- name: KOINON_AWSKMS_KEY_ID
  value: {{ $a.keyId | quote }}
{{- with $a.endpoint }}
- name: KOINON_AWSKMS_ENDPOINT
  value: {{ . | quote }}
{{- end }}
{{- with $a.stsEndpoint }}
- name: KOINON_AWSKMS_STS_ENDPOINT
  value: {{ . | quote }}
{{- end }}
{{- with $a.timeout }}
- name: KOINON_AWSKMS_TIMEOUT
  value: {{ . | quote }}
{{- end }}
{{- if $a.caConfigMap }}
- name: KOINON_AWSKMS_CA_FILE
  value: /etc/koinon/awskms-ca/ca.crt
{{- end }}
{{- end }}
{{- include "koinon.kmsReadOnlyEnv" (dict "root" .root "role" .role) }}
{{- end }}

{{/*
The read-only second wrapping key (values secrets.kek.kms.readOnly, issue 688 and 1298), as the KOINON_
forms of the --read-only-* flags: the KMS key that you used before a key move. It only unwraps; nothing new is
ever wrapped with it. It renders with any primary, local too (a move back to local). Each identity
signs in with its own service account token as its own role; nothing here is a credential. The key
must differ from the primary: koinon-api refuses to start with the same key twice, and the chart
refuses to render it.
*/}}
{{- define "koinon.kmsReadOnlyEnv" -}}
{{- $kms := .root.Values.secrets.kek.kms }}
{{- $ro := $kms.readOnly | default dict }}
{{- $b := $ro.backend | default "" }}
{{- if eq $b "openbao" }}
{{- $o := $ro.openbao }}
{{- if not (and $o.address $o.key) }}{{ fail "secrets.kek.kms.readOnly.openbao.address and key are required with readOnly.backend openbao" }}{{ end }}
{{- if and (eq (include "koinon.kms" .root) "openbao") (eq ($o.transitMount | default "transit") ($kms.openbao.transitMount | default "transit")) (eq $o.key $kms.openbao.key) }}
{{- fail "secrets.kek.kms.readOnly must name another key than the primary: set another transit mount or key name" }}
{{- end }}
{{- if and (hasPrefix "http://" $o.address) (not (regexMatch "^http://(127\\.[0-9.]+|localhost|\\[::1\\])(:[0-9]+)?/?$" $o.address)) }}
{{- fail "secrets.kek.kms.readOnly.openbao.address must be https: the KEKs travel to OpenBao (plain http only on a loopback address)" }}
{{- end }}
- name: KOINON_READ_ONLY_KEK_BACKEND
  value: openbao
- name: KOINON_READ_ONLY_OPENBAO_ADDRESS
  value: {{ $o.address | quote }}
- name: KOINON_READ_ONLY_OPENBAO_TRANSIT_MOUNT
  value: {{ $o.transitMount | default "transit" | quote }}
- name: KOINON_READ_ONLY_OPENBAO_KEY
  value: {{ $o.key | quote }}
- name: KOINON_READ_ONLY_OPENBAO_AUTH_MOUNT
  value: {{ $o.authMount | default "kubernetes" | quote }}
- name: KOINON_READ_ONLY_OPENBAO_ROLE
  value: {{ index $o.roles .role | quote }}
{{- with $o.namespace }}
- name: KOINON_READ_ONLY_OPENBAO_NAMESPACE
  value: {{ . | quote }}
{{- end }}
{{- with $o.timeout }}
- name: KOINON_READ_ONLY_OPENBAO_TIMEOUT
  value: {{ . | quote }}
{{- end }}
{{- if $o.caConfigMap }}
- name: KOINON_READ_ONLY_OPENBAO_CA_FILE
  value: /etc/koinon/ro-openbao-ca/ca.crt
{{- end }}
{{- else if eq $b "awskms" }}
{{- $a := $ro.awskms }}
{{- if not (and $a.region $a.keyId) }}{{ fail "secrets.kek.kms.readOnly.awskms.region and keyId are required with readOnly.backend awskms" }}{{ end }}
{{- if and (eq (include "koinon.kms" .root) "awskms") (eq $a.keyId $kms.awskms.keyId) }}
{{- fail "secrets.kek.kms.readOnly must name another key than the primary: set another AWS key ARN" }}
{{- end }}
{{- if hasPrefix "alias/aws/" $a.keyId }}
{{- fail "secrets.kek.kms.readOnly.awskms.keyId names an AWS managed key, whose key policy cannot limit who decrypts: use a customer managed key" }}
{{- end }}
{{- if and (hasPrefix "arn:" $a.keyId) (not (contains (printf ":kms:%s:" $a.region) $a.keyId)) }}
{{- fail "secrets.kek.kms.readOnly.awskms.keyId is a key ARN in another region than secrets.kek.kms.readOnly.awskms.region" }}
{{- end }}
{{- range $what, $e := dict "endpoint" $a.endpoint "stsEndpoint" $a.stsEndpoint }}
{{- if and (hasPrefix "http://" $e) (not (regexMatch "^http://(127\\.[0-9.]+|localhost|\\[::1\\])(:[0-9]+)?/?$" $e)) }}
{{- fail (printf "secrets.kek.kms.readOnly.awskms.%s must be https: the KEKs and signed requests travel to it (plain http only on a loopback address)" $what) }}
{{- end }}
{{- end }}
{{- /* Same as the primary: no credential and no AWS_* variable. The pod's one identity (IRSA role or
Pod Identity) needs kms:Decrypt on this key too. */}}
- name: KOINON_READ_ONLY_KEK_BACKEND
  value: awskms
- name: KOINON_READ_ONLY_AWSKMS_REGION
  value: {{ $a.region | quote }}
- name: KOINON_READ_ONLY_AWSKMS_KEY_ID
  value: {{ $a.keyId | quote }}
{{- with $a.endpoint }}
- name: KOINON_READ_ONLY_AWSKMS_ENDPOINT
  value: {{ . | quote }}
{{- end }}
{{- with $a.stsEndpoint }}
- name: KOINON_READ_ONLY_AWSKMS_STS_ENDPOINT
  value: {{ . | quote }}
{{- end }}
{{- with $a.timeout }}
- name: KOINON_READ_ONLY_AWSKMS_TIMEOUT
  value: {{ . | quote }}
{{- end }}
{{- if $a.caConfigMap }}
- name: KOINON_READ_ONLY_AWSKMS_CA_FILE
  value: /etc/koinon/ro-awskms-ca/ca.crt
{{- end }}
{{- else if $b }}
{{- fail (printf "secrets.kek.kms.readOnly.backend must be openbao or awskms (or empty), not %q" $b) }}
{{- end }}
{{- end }}

{{/*
The ConfigMap with the read-only key's CA when values name one, as name=ConfigMap, and its mount and
volume. Only the pods that start with the read-only flags (koinon-api, the reconciler, the key-admin
Job, the run-evidence Job) include these, next to the primary's.
*/}}
{{- define "koinon.kmsReadOnlyCA" -}}
{{- $ro := .Values.secrets.kek.kms.readOnly | default dict }}
{{- if and (eq ($ro.backend | default "") "openbao") $ro.openbao.caConfigMap }}ro-openbao-ca={{ $ro.openbao.caConfigMap }}
{{- else if and (eq ($ro.backend | default "") "awskms") $ro.awskms.caConfigMap }}ro-awskms-ca={{ $ro.awskms.caConfigMap }}
{{- end }}
{{- end }}

{{- define "koinon.kmsReadOnlyVolumeMount" -}}
{{- with include "koinon.kmsReadOnlyCA" . }}
{{- $v := splitList "=" . }}
- name: {{ index $v 0 }}
  mountPath: /etc/koinon/{{ index $v 0 }}
  readOnly: true
{{- end }}
{{- end }}

{{- define "koinon.kmsReadOnlyVolume" -}}
{{- with include "koinon.kmsReadOnlyCA" . }}
{{- $v := splitList "=" . }}
- name: {{ index $v 0 }}
  configMap:
    name: {{ index $v 1 }}
    items:
      - key: ca.crt
        path: ca.crt
{{- end }}
{{- end }}

{{/* The ConfigMap with the KMS's CA when values name one (a CA is not secret), and its volume name. */}}
{{- define "koinon.kmsCA" -}}
{{- $b := include "koinon.kms" . }}
{{- if and (eq $b "openbao") .Values.secrets.kek.kms.openbao.caConfigMap }}openbao-ca={{ .Values.secrets.kek.kms.openbao.caConfigMap }}
{{- else if and (eq $b "awskms") .Values.secrets.kek.kms.awskms.caConfigMap }}awskms-ca={{ .Values.secrets.kek.kms.awskms.caConfigMap }}
{{- end }}
{{- end }}

{{- define "koinon.kmsVolumeMount" -}}
{{- with include "koinon.kmsCA" . }}
{{- $v := splitList "=" . }}
- name: {{ index $v 0 }}
  mountPath: /etc/koinon/{{ index $v 0 }}
  readOnly: true
{{- end }}
{{- end }}

{{- define "koinon.kmsVolume" -}}
{{- with include "koinon.kmsCA" . }}
{{- $v := splitList "=" . }}
- name: {{ index $v 0 }}
  configMap:
    name: {{ index $v 1 }}
    items:
      - key: ca.crt
        path: ca.crt
{{- end }}
{{- end }}

{{/*
The annotations of an identity's ServiceAccount (dict root, role: api, reconciler or keyAdmin): with
the awskms backend and an IRSA role for it, eks.amazonaws.com/role-arn, which makes EKS's webhook give
the pod that role's web identity. While the primary is not awskms but the read-only key is (a move
away from AWS KMS), the read-only key's roleArns give it, so the pod can still unwrap. Empty otherwise.
*/}}
{{- define "koinon.kmsServiceAccountAnnotations" -}}
{{- $kms := .root.Values.secrets.kek.kms }}
{{- $ro := $kms.readOnly | default dict }}
{{- if eq (include "koinon.kms" .root) "awskms" }}
{{- with index $kms.awskms.roleArns .role }}
eks.amazonaws.com/role-arn: {{ . | quote }}
{{- end }}
{{- else if eq ($ro.backend | default "") "awskms" }}
{{- with index ($ro.awskms.roleArns | default dict) .role }}
eks.amazonaws.com/role-arn: {{ . | quote }}
{{- end }}
{{- end }}
{{- end }}

{{/*
The sandbox observer's ClusterRole (ADR 0078), named <namespace>-<fullname>-sandbox-observer like the
sandbox policies. The reconciler's RoleBinding to it in each workspace namespace has the same name.
*/}}
{{- define "koinon.observerRole" -}}
{{- printf "%s-%s-sandbox-observer" .Release.Namespace (include "koinon.fullname" .) -}}
{{- end }}

{{/*
The sandbox default-deny NetworkPolicy (spike 8.15, ADR 0045), named
<namespace>-<fullname>-sandbox-default-deny like the sandbox policies. The reconciler
creates one of this name in each workspace namespace.
*/}}
{{- define "koinon.sandboxDefaultDeny" -}}
{{- printf "%s-%s-sandbox-default-deny" .Release.Namespace (include "koinon.fullname" .) -}}
{{- end }}

{{/*
The sandbox LimitRange (#76, ADR 0083 follow-up), named
<namespace>-<fullname>-sandbox-limitrange like the sandbox policies. The reconciler
creates one of this name in each workspace namespace.
*/}}
{{- define "koinon.sandboxLimitRange" -}}
{{- printf "%s-%s-sandbox-limitrange" .Release.Namespace (include "koinon.fullname" .) -}}
{{- end }}

{{/*
koinon.quantity turns a Kubernetes quantity (250m, 2, 1.5Gi, 512M, 1e3) into a float64 in base units, so
koinon.validate can compare two of them. An empty string gives -1 (a bound that is not set); text that
is not a quantity fails the render with the same view as koinon-api would give at start. The grammar is
that of k8s resource.ParseQuantity: a render must never refuse a value the binaries accept.
*/}}
{{- define "koinon.quantity" -}}
{{- $q := toString .q -}}
{{- if eq $q "" -}}-1
{{- else -}}
{{- if not (regexMatch "^[+-]?([0-9]+(\\.[0-9]*)?|\\.[0-9]+)(m|n|u|k|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei|[eE][+-]?[0-9]+)?$" $q) }}{{- fail (printf "%s %q is not a Kubernetes quantity (examples: 250m, 2, 1Gi)" .what $q) }}{{- end -}}
{{- $num := regexFind "^[+-]?([0-9]+(\\.[0-9]*)?|\\.[0-9]+)" $q -}}
{{- $unit := trimPrefix $num $q -}}
{{- $n := float64 $num -}}
{{- if regexMatch "^[eE][+-]?[0-9]+$" $unit -}}
{{- float64 $q -}}
{{- else -}}
{{- $f := dict "" 1.0 "n" 1e-9 "u" 1e-6 "m" 0.001 "k" 1e3 "M" 1e6 "G" 1e9 "T" 1e12 "P" 1e15 "E" 1e18 "Ki" 1024.0 "Mi" 1048576.0 "Gi" 1073741824.0 "Ti" 1099511627776.0 "Pi" 1125899906842624.0 "Ei" 1152921504606846976.0 -}}
{{- mulf $n (index $f $unit) -}}
{{- end -}}
{{- end -}}
{{- end }}

{{- define "koinon.validate" -}}
{{- /* sandboxres.Policy.Validate (koinon-api and the reconciler refuse to start on these two cross-field rules): a limit is clamped into its range and the request is then lowered to it, so the limit range must not start below the request minimum; with memoryRequestEqualsLimit it must not end above the request maximum. */ -}}
{{- $sb := .Values.sandbox.resources }}
{{- range $r := list (dict "name" "cpu" "b" $sb.bounds.cpu "equal" false) (dict "name" "memory" "b" $sb.bounds.memory "equal" $sb.memoryRequestEqualsLimit) }}
{{- $rm := $r.b.request.min | toString }}
{{- if $rm }}
{{- $lm := $r.b.limit.min | toString }}
{{- $rv := include "koinon.quantity" (dict "q" $rm "what" (printf "sandbox.resources.bounds.%s.request.min" $r.name)) | float64 }}
{{- $lv := include "koinon.quantity" (dict "q" $lm "what" (printf "sandbox.resources.bounds.%s.limit.min" $r.name)) | float64 }}
{{- if not $lm }}
{{- fail (printf "sandbox.resources.bounds.%s: request.min is %q but limit.min is empty and must be set: a limit range must not start below the request minimum, or koinon-api and the reconciler refuse to start. Set limit.min to at least request.min" $r.name $rm) }}
{{- end }}
{{- if lt $lv $rv }}
{{- fail (printf "sandbox.resources.bounds.%s: limit.min %q is below request.min %q: a limit range must not start below the request minimum, or koinon-api and the reconciler refuse to start. Raise limit.min or lower request.min" $r.name $lm $rm) }}
{{- end }}
{{- end }}
{{- $rx := $r.b.request.max | toString }}
{{- if and $r.equal $rx }}
{{- $lx := $r.b.limit.max | toString }}
{{- $rv := include "koinon.quantity" (dict "q" $rx "what" (printf "sandbox.resources.bounds.%s.request.max" $r.name)) | float64 }}
{{- $lv := include "koinon.quantity" (dict "q" $lx "what" (printf "sandbox.resources.bounds.%s.limit.max" $r.name)) | float64 }}
{{- if not $lx }}
{{- fail (printf "sandbox.resources.bounds.%s: with sandbox.resources.memoryRequestEqualsLimit=true request.max is %q but limit.max is empty and must be set: an empty limit range has no end, or koinon-api and the reconciler refuse to start. Set limit.max to at most request.max, or set memoryRequestEqualsLimit=false" $r.name $rx) }}
{{- end }}
{{- if gt $lv $rv }}
{{- fail (printf "sandbox.resources.bounds.%s: with sandbox.resources.memoryRequestEqualsLimit=true limit.max %q is above request.max %q, or koinon-api and the reconciler refuse to start. Lower limit.max, raise request.max, or set memoryRequestEqualsLimit=false" $r.name $lx $rx) }}
{{- end }}
{{- end }}
{{- end }}
{{- if eq (include "koinon.kms" .) "openbao" }}
{{- $o := .Values.secrets.kek.kms.openbao }}
{{- if not $o.address }}
{{- fail "secrets.kek.kms.openbao.address is required when secrets.kek.kms.backend is openbao" }}
{{- end }}
{{- if not $o.key }}
{{- fail "secrets.kek.kms.openbao.key is required when secrets.kek.kms.backend is openbao: the transit key that wraps the KEKs" }}
{{- end }}
{{- if and (hasPrefix "http://" $o.address) (not (regexMatch "^http://(127\\.[0-9.]+|localhost|\\[::1\\])(:[0-9]+)?/?$" $o.address)) }}
{{- fail "secrets.kek.kms.openbao.address must be https: the KEKs travel to OpenBao (plain http only on a loopback address)" }}
{{- end }}
{{- end }}
{{- if eq (include "koinon.kms" .) "awskms" }}
{{- $a := .Values.secrets.kek.kms.awskms }}
{{- if not $a.region }}
{{- fail "secrets.kek.kms.awskms.region is required when secrets.kek.kms.backend is awskms" }}
{{- end }}
{{- if not $a.keyId }}
{{- fail "secrets.kek.kms.awskms.keyId is required when secrets.kek.kms.backend is awskms: the KMS key that wraps the KEKs" }}
{{- end }}
{{- if hasPrefix "alias/aws/" $a.keyId }}
{{- fail "secrets.kek.kms.awskms.keyId names an AWS managed key, whose key policy cannot limit who decrypts: use a customer managed key" }}
{{- end }}
{{- if and (hasPrefix "arn:" $a.keyId) (not (contains (printf ":kms:%s:" $a.region) $a.keyId)) }}
{{- fail "secrets.kek.kms.awskms.keyId is a key ARN in another region than secrets.kek.kms.awskms.region" }}
{{- end }}
{{- range $what, $e := dict "endpoint" $a.endpoint "stsEndpoint" $a.stsEndpoint }}
{{- if and (hasPrefix "http://" $e) (not (regexMatch "^http://(127\\.[0-9.]+|localhost|\\[::1\\])(:[0-9]+)?/?$" $e)) }}
{{- fail (printf "secrets.kek.kms.awskms.%s must be https: the KEKs and signed requests travel to it (plain http only on a loopback address)" $what) }}
{{- end }}
{{- end }}
{{- /* IRSA for every identity, or for none (EKS Pod Identity): a partial set leaves a pod without its role. */ -}}
{{- $roles := $a.roleArns | default dict }}
{{- $set := 0 }}
{{- range $r := list "api" "reconciler" "keyAdmin" }}{{ if index $roles $r }}{{ $set = add1 $set }}{{ end }}{{ end }}
{{- if and (gt $set 0) (lt $set 3) }}
{{- fail "secrets.kek.kms.awskms.roleArns: set api, reconciler and keyAdmin together (IRSA), or none of them (EKS Pod Identity)" }}
{{- end }}
{{- end }}
{{- if and .Values.secrets.kek.kms.keyAdmin.addKey (not (include "koinon.kms" .)) }}
{{- fail "secrets.kek.kms.keyAdmin.addKey needs a KMS (secrets.kek.kms.backend): a local install adds KEKs with koinon-api keys add" }}
{{- end }}
{{- if and .Values.secrets.kek.kms.keyAdmin.activateIfNoActive (not .Values.secrets.kek.kms.keyAdmin.addKey) }}
{{- fail "secrets.kek.kms.keyAdmin.activateIfNoActive needs secrets.kek.kms.keyAdmin.addKey: the new ID it makes active" }}
{{- end }}
{{- if .Values.sandboxObserver.enabled }}
{{- if not .Values.sandboxPolicy.enabled }}
{{- fail "sandboxObserver.enabled needs sandboxPolicy.enabled: the policy's RoleBinding rule is what limits the reconciler's right to bind the observer role to sandbox namespaces. Enable sandboxPolicy, or set sandboxObserver.enabled=false (a sandbox that cannot pull its image then stays an unknown wait cause)" }}
{{- end }}
{{- if not (has "Deny" .Values.sandboxPolicy.validationActions) }}
{{- fail "sandboxObserver.enabled needs sandboxPolicy.validationActions to include Deny: without it the policy only warns or audits and does not limit where the reconciler may bind the observer role. Set sandboxObserver.enabled=false to run without the observer" }}
{{- end }}
{{- end }}
{{- if .Values.sandboxNetworkPolicy.enabled }}
{{- if not .Values.sandboxPolicy.enabled }}
{{- fail "sandboxNetworkPolicy.enabled needs sandboxPolicy.enabled: the NetworkPolicy writer's ValidatingAdmissionPolicy uses sandboxPolicy.namespaceSelector to say which namespaces are sandbox namespaces. Enable sandboxPolicy, or set sandboxNetworkPolicy.enabled=false" }}
{{- end }}
{{- if not (has "Deny" .Values.sandboxPolicy.validationActions) }}
{{- fail "sandboxNetworkPolicy.enabled needs sandboxPolicy.validationActions to include Deny: without it the writer ClusterRole can create NetworkPolicies cluster-wide while the policy only warns or audits. Set sandboxNetworkPolicy.enabled=false to run without the writer" }}
{{- end }}
{{- end }}
{{- if .Values.sandboxLimitRange.enabled }}
{{- if not .Values.sandboxPolicy.enabled }}
{{- fail "sandboxLimitRange.enabled needs sandboxPolicy.enabled: the LimitRange writer's ValidatingAdmissionPolicy uses sandboxPolicy.namespaceSelector to say which namespaces are sandbox namespaces. Enable sandboxPolicy, or set sandboxLimitRange.enabled=false" }}
{{- end }}
{{- if not (has "Deny" .Values.sandboxPolicy.validationActions) }}
{{- fail "sandboxLimitRange.enabled needs sandboxPolicy.validationActions to include Deny: without it the writer ClusterRole can create LimitRanges cluster-wide while the policy only warns or audits. Set sandboxLimitRange.enabled=false to run without the writer" }}
{{- end }}
{{- end }}
{{- if .Values.checkpoints.enabled }}
{{- if not .Values.checkpoints.s3.existingSecret }}
{{- fail "checkpoints.s3.existingSecret is required when checkpoints.enabled is true: a Secret with AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, endpoint and region" }}
{{- end }}
{{- if not .Values.checkpoints.s3.bucket }}
{{- fail "checkpoints.s3.bucket is required when checkpoints.enabled is true" }}
{{- end }}
{{- if not .Values.checkpoints.s3.pathStyle }}
{{- fail "checkpoints.s3.pathStyle=false is not supported: the S3 client addresses buckets path-style except on AWS" }}
{{- end }}
{{- end }}
{{- if and .Values.harnessPrepull.enabled (not .Values.harnessPrepull.nodeSelector) }}
{{- fail "harnessPrepull.nodeSelector is required when harnessPrepull.enabled is true: select the Kata nodes, or the images are pulled onto every node" }}
{{- end }}
{{- if not (has .Values.postgres.mode (list "cnpg" "external")) }}
{{- fail "postgres.mode must be cnpg or external" }}
{{- end }}
{{- if not (has .Values.gateway.mode (list "standalone" "kubernetes")) }}
{{- fail "gateway.mode must be standalone or kubernetes" }}
{{- end }}
{{- if and (eq .Values.postgres.mode "external") (not .Values.postgres.external.secretName) }}
{{- fail "postgres.external.secretName is required when postgres.mode is external" }}
{{- end }}
{{- if and .Values.ingress.enabled (not .Values.ingress.host) }}
{{- fail "ingress.host is required when ingress.enabled is true" }}
{{- end }}
{{- if and .Values.httpRoute.enabled (or (not .Values.httpRoute.hostnames) (not .Values.httpRoute.parentRefs)) }}
{{- fail "httpRoute.hostnames and httpRoute.parentRefs are required when httpRoute.enabled is true" }}
{{- end }}
{{- if and .Values.httpRoute.enabled .Values.ingress.enabled }}
{{- fail "ingress.enabled and httpRoute.enabled are both set: expose Koinon through one of them" }}
{{- end }}
{{- if and .Values.httpRoute.enabled .Values.httpRoute.hostnames (not .Values.publicUrl) (contains "*" (first .Values.httpRoute.hostnames)) }}
{{- fail "the first httpRoute hostname is a wildcard, so it cannot be the public URL: set publicUrl" }}
{{- end }}
{{- if not (has (.Values.execution.driver | default "openshell") (list "openshell" "substrate")) }}
{{- fail (printf "execution.driver %q: want openshell or substrate" .Values.execution.driver) }}
{{- end }}
{{- if eq (include "koinon.substrate" .) "true" }}
{{- if or (not .Values.execution.substrate.controlAddress) (not .Values.execution.substrate.routerAddress) }}
{{- fail "execution.substrate.controlAddress and execution.substrate.routerAddress are required with execution.driver=substrate" }}
{{- end }}
{{- if not .Values.execution.substrate.tokenAudience }}
{{- fail "execution.substrate.tokenAudience is required with execution.driver=substrate" }}
{{- end }}
{{- /* Issue #1489, ADR 0120 amendment of 2026-11-27: the guest holds no gateway key; atenet-egress adds the actor JWT,
and only koinon-gateway checks it. agentgateway cannot, so the model and MCP paths must be koinon-gateway's. */}}
{{- if eq .Values.gateway.mode "kubernetes" }}
{{- fail "gateway.mode=kubernetes is not supported with execution.driver=substrate in v0.1.0: the guest holds no gateway key, and only koinon-gateway checks the actor JWT that atenet-egress adds to a model or MCP call, so agentgateway cannot carry them (issue #1489, ADR 0097 amendment of 2026-11-27). Use gateway.mode=standalone with gateway.model.driver=koinon and gateway.mcp.driver=koinon" }}
{{- end }}
{{- if not (include "koinon.modelKoinon" .) }}
{{- fail "execution.driver=substrate needs gateway.model.driver=koinon (the default with Substrate): the guest holds no gateway key, and only koinon-gateway checks the actor JWT that atenet-egress adds to a model call (issue #1489). Remove gateway.model.driver=agentgateway. gateway.mode=kubernetes is not supported with the Substrate driver in v0.1.0 (ADR 0097 amendment of 2026-11-27)" }}
{{- end }}
{{- if not (include "koinon.mcpKoinon" .) }}
{{- fail "execution.driver=substrate needs gateway.mcp.driver=koinon: the guest holds no gateway key, and only koinon-gateway checks the actor JWT that atenet-egress adds to an MCP call (issue #1489). Set gateway.mode=standalone: gateway.mode=kubernetes is not supported with the Substrate driver in v0.1.0 (ADR 0097 amendment of 2026-11-27)" }}
{{- end }}
{{- if not .Values.execution.substrate.allowUnhardened }}
{{- fail "execution.driver=substrate needs execution.substrate.allowUnhardened=true: the substrate overlay of the harness image (guest server and guest launcher, issue #1087) has not yet run in a real Substrate actor with its probe results recorded. The gVisor tiers keep a writable root (issue #1150). Open items that the flag acknowledges are listed in docs/dev/guest-launcher.md (Open items 2, 4 and 7: probe results not recorded, payload runs as root, writable root on the gVisor tiers without Landlock)" }}
{{- end }}
{{- if and (not .Values.execution.substrate.defaultTier) (not .Values.execution.substrate.runtimeClassTiers) }}
{{- fail "execution.substrate.defaultTier or execution.substrate.runtimeClassTiers is required with execution.driver=substrate: the driver starts no guest process for a sandbox without an isolation tier" }}
{{- end }}
{{- range $class, $tier := .Values.execution.substrate.runtimeClassTiers }}
{{- if not (has $tier (list "vm" "hardened" "dev")) }}
{{- fail (printf "execution.substrate.runtimeClassTiers.%s %q: want vm, hardened or dev" $class $tier) }}
{{- end }}
{{- end }}
{{- if and .Values.execution.substrate.defaultTier (not (has .Values.execution.substrate.defaultTier (list "vm" "hardened" "dev"))) }}
{{- fail (printf "execution.substrate.defaultTier %q: want vm, hardened or dev" .Values.execution.substrate.defaultTier) }}
{{- end }}
{{- range $k := list "sandboxObserver" "sandboxNetworkPolicy" "sandboxLimitRange" }}
{{- if (index $.Values $k).enabled }}
{{- fail (printf "%s.enabled acts on OpenShell's namespaces and pods: set it to false with execution.driver=substrate" $k) }}
{{- end }}
{{- end }}
{{- end }}
{{- if and (ne (include "koinon.substrate" .) "true") (not .Values.sandboxNetworkPolicy.enabled) .Values.sandboxPolicy.enabled }}
{{- fail "execution.driver=openshell with sandboxPolicy.enabled needs sandboxNetworkPolicy.enabled=true (and, for the full OpenShell setup, sandboxObserver.enabled=true and sandboxLimitRange.enabled=true): the default-deny NetworkPolicy of ADR 0045 is applied only when it is on. Substrate is the default driver; the three values default to false for it" }}
{{- end }}
{{- if and (ne (include "koinon.substrate" .) "true") (not .Values.openshell.clientTLSSecret) (not .Values.openshell.insecure) }}
{{- fail "openshell.clientTLSSecret is required: Koinon dials the OpenShell gateway (it controls every sandbox) over mTLS. Set openshell.clientTLSSecret to a Secret with the gateway's client certificate (tls.crt, tls.key, ca.crt), or set openshell.insecure=true only if a service mesh provides mTLS between Koinon and the gateway" }}
{{- end }}
{{- if and (ne (include "koinon.substrate" .) "true") .Values.openshell.clientTLSSecret .Values.openshell.insecure }}
{{- fail "openshell.clientTLSSecret and openshell.insecure=true contradict each other: set the Secret for mTLS, or insecure=true for plaintext, not both" }}
{{- end }}
{{- if and .Values.oidc.enabled (not .Values.oidc.redirectUrl) (not (include "koinon.publicUrl" .)) }}
{{- fail "oidc.enabled needs oidc.redirectUrl, publicUrl, an ingress or an httpRoute to build the redirect URL from" }}
{{- end }}
{{- end }}

{{/*
koinon-agentd's settings (plan item AG1). The gateway URL tells sandboxes where agentgateway is;
it carries no credentials.
*/}}
{{/* The Secret koinon-api creates on its first checkpoint for the Ed25519 key that signs audit checkpoints (ADR 0100 decision 4.2). Helm never renders it, so an upgrade never overwrites the key. Only koinon-api reads it, through the Kubernetes API: no workload mounts it and no Role other than koinon-api's names it. Rotation: delete it (ADR 0100, signed checkpoints). */}}
{{- define "koinon.auditSigningSecretName" -}}
{{- include "koinon.componentName" (list . "audit-signing") -}}
{{- end }}
{{/* The Secret koinon-api creates on first use for the key that signs saved-work uploads (ADR 0054). koinon-agentd mounts it read-only (optional: it exists once koinon-api has started); it is not a KEK. */}}
{{- define "koinon.savedWorkSecretName" -}}
{{- include "koinon.componentName" (list . "saved-work") -}}
{{- end }}

{{- define "koinon.savedWorkVolumeMount" -}}
- name: saved-work
  mountPath: /secrets/saved-work
  readOnly: true
{{- end }}

{{- define "koinon.savedWorkVolume" -}}
- name: saved-work
  secret:
    secretName: {{ include "koinon.savedWorkSecretName" . }}
    defaultMode: 0440
    optional: true
{{- end }}

{{- define "koinon.agentdEnv" -}}
{{- /* The internal OTLP listener, for the run spans agentd exports (ADR 0074): its address and the bearer token it requires for /v1/traces (issue 55: not the gateway's). */}}
- name: KOINON_OTLP_ENDPOINT
  value: {{ printf "http://%s.%s.svc.%s:%v" (include "koinon.componentName" (list . "api")) .Release.Namespace (.Values.clusterDomain | default "cluster.local") .Values.api.otlpPort | quote }}
- name: KOINON_OTLP_TOKEN_FILE
  value: {{ printf "%s/token" (include "koinon.otlpTracesTokenDir" .) | quote }}
{{- /* The uncommitted work of a stuck sandbox is posted here to be sealed; agentd holds no KEK (ADR 0054). */}}
- name: KOINON_SAVED_WORK_KEY_FILE
  value: /secrets/saved-work/key
- name: KOINON_SAVED_WORK_URL
  value: {{ printf "http://%s.%s.svc.%s:%v" (include "koinon.componentName" (list . "api-saved-work")) .Release.Namespace (.Values.clusterDomain | default "cluster.local") .Values.api.savedWorkPort | quote }}
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
{{- if eq (include "koinon.substrate" .) "true" }}
- name: KOINON_EXECUTION_DRIVER
  value: substrate
- name: KOINON_SUBSTRATE_CONTROL_ADDRESS
  value: {{ .Values.execution.substrate.controlAddress | quote }}
- name: KOINON_SUBSTRATE_ROUTER_ADDRESS
  value: {{ .Values.execution.substrate.routerAddress | quote }}
- name: KOINON_SUBSTRATE_ROUTER_CERT_FILE
  value: {{ printf "%s/credential-bundle.pem" (include "koinon.substrateRouterDir" .) | quote }}
- name: KOINON_SUBSTRATE_ROUTER_CA_FILE
  value: {{ printf "%s/router-ca.pem" (include "koinon.substrateRouterDir" .) | quote }}
{{- with .Values.execution.substrate.routerServerName }}
- name: KOINON_SUBSTRATE_ROUTER_SERVER_NAME
  value: {{ . | quote }}
{{- end }}
- name: KOINON_SUBSTRATE_ALLOW_UNHARDENED
  value: {{ .Values.execution.substrate.allowUnhardened | default false | quote }}
{{- with .Values.execution.substrate.defaultTier }}
- name: KOINON_SUBSTRATE_DEFAULT_TIER
  value: {{ . | quote }}
{{- end }}
- name: KOINON_SUBSTRATE_RUNTIME_CLASS_TIERS
  value: {{ include "koinon.substrateTiers" . | quote }}
- name: KOINON_SUBSTRATE_TOKEN_FILE
  value: {{ printf "%s/token" (include "koinon.substrateTokenDir" .) | quote }}
- name: KOINON_SUBSTRATE_GUEST_KEY_FILE
  value: {{ include "koinon.substrateGuestKeyFile" . | quote }}
{{- if .Values.execution.substrate.caSecret }}
- name: KOINON_SUBSTRATE_CA_FILE
  value: /etc/koinon/substrate-ca/ca.crt
{{- end }}
{{- with .Values.execution.substrate.serverName }}
- name: KOINON_SUBSTRATE_SERVER_NAME
  value: {{ . | quote }}
{{- end }}
{{- else }}
- name: KOINON_OPENSHELL_ADDRESS
  value: {{ include "koinon.openshellAddress" . | quote }}
{{- if .Values.openshell.clientTLSSecret }}
- name: KOINON_OPENSHELL_TLS_DIR
  value: /etc/koinon/openshell-tls
{{- end }}
{{- end }}
{{- $gatewayURL := .Values.agentd.gatewayURL }}
{{- if and (not $gatewayURL) (eq (include "koinon.substrate" .) "true") }}
{{- /* Substrate (issue #1489): the agents' model calls go to koinon-gateway's model path, which checks the actor JWT. */}}
{{- $gatewayURL = include "koinon.modelGatewayURL" . }}
{{- end }}
{{- if not $gatewayURL }}
{{- $gatewayURL = printf "http://%s:%s" (include "koinon.gatewayHost" .) (include "koinon.gatewayPort" .) }}
{{- end }}
{{- with $gatewayURL }}
- name: KOINON_GATEWAY_URL
  value: {{ . | quote }}
{{- end }}
{{- if include "koinon.egressListener" . }}
{{- /* Git through the gateway's egress listener (#1086 part C2). On openshell agentd mints the actor token with the seed, outside the guest. On substrate atenet-egress adds the token, so agentd gets no key (issue #1386). */}}
- name: KOINON_EGRESS_URL
  value: {{ include "koinon.egressURL" . | quote }}
{{- if include "koinon.ed25519ActorToken" . }}
- name: KOINON_ACTOR_TOKEN_KEY_FILE
  value: /secrets/actor-token/key
- name: KOINON_ACTOR_TOKEN_KEY_ID
  value: {{ .Values.gateway.mcp.koinon.egress.actorToken.keyId | quote }}
- name: KOINON_ACTOR_TOKEN_ISSUER
  value: {{ .Values.gateway.mcp.koinon.egress.actorToken.issuer | quote }}
{{- end }}
{{- end }}
{{- with include "koinon.agentMCPURL" . }}
{{- /* gateway.mcp.driver=koinon: the agents' MCP calls go to koinon-gateway; the model path stays on KOINON_GATEWAY_URL (ADR 0097 decision 5, #1239). */}}
- name: KOINON_AGENT_MCP_URL
  value: {{ . | quote }}
{{- end }}
- name: KOINON_WORKERS
  value: {{ .Values.agentd.workers | default 8 | quote }}
- name: KOINON_LEASE
  value: {{ .Values.agentd.lease | default "30s" | quote }}
- name: KOINON_DRAIN_DEADLINE
  value: {{ printf "%ds" (int (.Values.agentd.drainDeadlineSeconds | default 300)) | quote }}
{{- if .Values.checkpoints.enabled }}
{{- /* Home checkpoints: agentd is the writer, the only holder of the S3 credentials, and holds no KEK: koinon-api hands it each repository password (ADR 0032, 0054). */}}
- name: KOINON_CHECKPOINTS
  value: "true"
- name: KOINON_CHECKPOINTS_INTERVAL
  value: {{ .Values.checkpoints.interval | quote }}
{{- if .Values.checkpoints.readOnly }}
- name: KOINON_CHECKPOINTS_READ_ONLY
  value: "true"
{{- end }}
- name: KOINON_CHECKPOINTS_S3_BUCKET
  value: {{ .Values.checkpoints.s3.bucket | quote }}
{{- range $env, $key := dict "KOINON_CHECKPOINTS_S3_ENDPOINT" "endpoint" "KOINON_CHECKPOINTS_S3_REGION" "region" "AWS_ACCESS_KEY_ID" "AWS_ACCESS_KEY_ID" "AWS_SECRET_ACCESS_KEY" "AWS_SECRET_ACCESS_KEY" }}
- name: {{ $env }}
  valueFrom:
    secretKeyRef:
      name: {{ $.Values.checkpoints.s3.existingSecret | quote }}
      key: {{ $key }}
{{- end }}
{{- end }}
{{- end }}

{{/*
The key-admin Job's pod template (key-admin.yaml), as a string: the Job is named by its hash.
dict root, sa (service account), kek (Secret name), create (bool), ka (values secrets.kek.kms.keyAdmin).
*/}}
{{- define "koinon.keyAdminPod" -}}
{{- $ := .root -}}
metadata:
  labels:
    {{- include "koinon.labels" (list $ "key-admin") | nindent 4 }}
spec:
  restartPolicy: Never
  serviceAccountName: {{ .sa }}
  # The token signs in to the KMS (Kubernetes auth) and reaches the Kubernetes API.
  automountServiceAccountToken: true
  enableServiceLinks: false
  {{- with include "koinon.imagePullSecrets" $ | trim }}
  {{- . | nindent 2 }}
  {{- end }}
  securityContext:
    {{- include "koinon.podSecurityContext" $ | nindent 4 }}
  containers:
    - name: key-admin
      image: {{ include "koinon.image" (list $ $.Values.api.image) }}
      imagePullPolicy: {{ $.Values.image.pullPolicy }}
      args:
        - key-admin
        - {{ printf "--kek-secret=%s" .kek | quote }}
        {{- if .create }}
        - --kek-create
        {{- end }}
        {{- with .ka.addKey }}
        - {{ printf "--add-key=%s" . | quote }}
        {{- end }}
        {{- if .ka.activateIfNoActive }}
        - --activate-if-no-active
        {{- end }}
        {{- if .ka.rewrapUnbound }}
        - --rewrap-unbound
        {{- end }}
      env:
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        # Always: every KEK it wraps is bound to the install ID in the database (old issue 615).
        {{- include "koinon.dbEnv" (list $ "api") | nindent 8 }}
        {{- include "koinon.kmsEnv" (dict "root" $ "role" "keyAdmin") | trim | nindent 8 }}
      securityContext:
        {{- include "koinon.containerSecurityContext" $ | nindent 8 }}
      terminationMessagePolicy: FallbackToLogsOnError
      resources:
        {{- toYaml .ka.resources | nindent 8 }}
      {{- $mounts := list (include "koinon.dbVolumeMount" $) (include "koinon.kmsVolumeMount" $) (include "koinon.kmsReadOnlyVolumeMount" $) | join "\n" | trim }}
      {{- $volumes := list (include "koinon.dbVolume" $) (include "koinon.kmsVolume" $) (include "koinon.kmsReadOnlyVolume" $) | join "\n" | trim }}
      {{- with $mounts }}
      volumeMounts:
        {{- . | nindent 8 }}
      {{- end }}
  {{- with $volumes }}
  volumes:
    {{- . | nindent 4 }}
  {{- end }}
{{- end }}

{{/*
The run-evidence export Job's pod template (run-evidence-export.yaml), as a string: the Job is named by its hash.
dict root, re (values runEvidence.export).
*/}}
{{- define "koinon.runEvidencePod" -}}
{{- $ := .root -}}
{{- $re := .re -}}
metadata:
  labels:
    {{- include "koinon.labels" (list $ "run-evidence") | nindent 4 }}
spec:
  restartPolicy: Never
  serviceAccountName: {{ include "koinon.componentName" (list $ "api") }}
  # The token reads the KEK Secret through the Kubernetes API.
  automountServiceAccountToken: true
  enableServiceLinks: false
  {{- with include "koinon.imagePullSecrets" $ | trim }}
  {{- . | nindent 2 }}
  {{- end }}
  securityContext:
    {{- include "koinon.podSecurityContext" $ | nindent 4 }}
  containers:
    - name: run-evidence
      image: {{ include "koinon.image" (list $ $.Values.api.image) }}
      imagePullPolicy: {{ $.Values.image.pullPolicy }}
      args:
        - run-evidence
        - export
        - {{ printf "--org=%s" $re.org | quote }}
        - {{ printf "--run=%s" $re.run | quote }}
        - {{ printf "--out=%s" $re.out | quote }}
        - {{ printf "--kek-secret=%s" (include "koinon.kekSecretName" $) | quote }}
        - --s3-endpoint=$(S3_ENDPOINT)
        - --s3-region=$(S3_REGION)
      env:
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        {{- range $env, $key := dict "S3_ENDPOINT" "endpoint" "S3_REGION" "region" "AWS_ACCESS_KEY_ID" "AWS_ACCESS_KEY_ID" "AWS_SECRET_ACCESS_KEY" "AWS_SECRET_ACCESS_KEY" }}
        - name: {{ $env }}
          valueFrom:
            secretKeyRef:
              name: {{ $re.s3.existingSecret | quote }}
              key: {{ $key }}
        {{- end }}
        {{- include "koinon.dbEnv" (list $ "api") | nindent 8 }}
        {{- include "koinon.kmsEnv" (dict "root" $ "role" "api") | trim | nindent 8 }}
      securityContext:
        {{- include "koinon.containerSecurityContext" $ | nindent 8 }}
      terminationMessagePolicy: FallbackToLogsOnError
      resources:
        {{- toYaml $re.resources | nindent 8 }}
      {{- $mounts := list (include "koinon.dbVolumeMount" $) (include "koinon.kmsVolumeMount" $) (include "koinon.kmsReadOnlyVolumeMount" $) | join "\n" | trim }}
      {{- $volumes := list (include "koinon.dbVolume" $) (include "koinon.kmsVolume" $) (include "koinon.kmsReadOnlyVolume" $) | join "\n" | trim }}
      {{- with $mounts }}
      volumeMounts:
        {{- . | nindent 8 }}
      {{- end }}
  {{- with $volumes }}
  volumes:
    {{- . | nindent 4 }}
  {{- end }}
{{- end }}

{{/*
koinon.pricesRefreshMode is koinon-api's --prices-refresh (ADR 0098, maintainer decision 2026-11-07):
prices.refresh.enabled unset is auto (koinon-api fetches OpenRouter only while an openrouter provider
exists; the chart knows nothing about providers), false is off, true is on.
*/}}
{{- define "koinon.pricesRefreshMode" -}}
{{- $e := .Values.prices.refresh.enabled -}}
{{- if not (kindIs "bool" $e) -}}auto{{- else if $e -}}on{{- else -}}off{{- end -}}
{{- end }}

{{/*
koinon.sandboxNamespaceSelector is the namespace selector the sandbox admission policies bind to
(YAML): sandboxPolicy.namespaceSelector plus openshell.ai/gateway-id=<openshell.gatewayId>, so a
release's policies cover only its own gateway's workspace namespaces and not those of another
Koinon release's gateway on the same cluster (old issue 460). sandboxPolicy.scopeToGateway=false
keeps the selector as set (every OpenShell namespace).
*/}}
{{- define "koinon.sandboxNamespaceSelector" -}}
{{- $sel := deepCopy .Values.sandboxPolicy.namespaceSelector -}}
{{- if .Values.sandboxPolicy.scopeToGateway -}}
{{- $labels := $sel.matchLabels | default dict -}}
{{- $_ := set $labels "openshell.ai/gateway-id" .Values.openshell.gatewayId -}}
{{- $_ := set $sel "matchLabels" $labels -}}
{{- end -}}
{{- toYaml $sel -}}
{{- end }}

{{/*
koinon.sandboxUnlabelledSelector is the selector of the extra bindings that keep an OpenShell
namespace WITHOUT the openshell.ai/gateway-id label deny-by-default while the policies are scoped to
the gateway: no release's gateway owns such a namespace, so every release covers it. It is
sandboxPolicy.namespaceSelector plus gateway-id DoesNotExist (old issue 460).
*/}}
{{- define "koinon.sandboxUnlabelledSelector" -}}
{{- $sel := deepCopy .Values.sandboxPolicy.namespaceSelector -}}
{{- $exprs := $sel.matchExpressions | default list -}}
{{- $_ := set $sel "matchExpressions" (append $exprs (dict "key" "openshell.ai/gateway-id" "operator" "DoesNotExist")) -}}
{{- toYaml $sel -}}
{{- end }}

{{/*
koinon.selectorCEL renders a label selector (a dict with matchLabels and matchExpressions) as a CEL
condition over a map variable of labels: (list $selector "variables.oldLabels"). It is the same
selector semantics as a namespaceSelector, for a policy that has to judge a namespace's labels
itself (old issue 475). Operators other than In, NotIn, Exists and DoesNotExist fail the render.
*/}}
{{- define "koinon.selectorCEL" -}}
{{- $sel := index . 0 -}}
{{- $v := index . 1 -}}
{{- $conds := list -}}
{{- range $k, $val := ($sel.matchLabels | default dict) -}}
{{- $conds = append $conds (printf "(%s in %s && %s[%s] == %s)" ($k | quote) $v $v ($k | quote) ($val | quote)) -}}
{{- end -}}
{{- range ($sel.matchExpressions | default list) -}}
{{- $k := .key | quote -}}
{{- if eq .operator "In" -}}
{{- $conds = append $conds (printf "(%s in %s && %s[%s] in %s)" $k $v $v $k (.values | toJson)) -}}
{{- else if eq .operator "NotIn" -}}
{{- $conds = append $conds (printf "(!(%s in %s) || !(%s[%s] in %s))" $k $v $v $k (.values | toJson)) -}}
{{- else if eq .operator "Exists" -}}
{{- $conds = append $conds (printf "(%s in %s)" $k $v) -}}
{{- else if eq .operator "DoesNotExist" -}}
{{- $conds = append $conds (printf "(!(%s in %s))" $k $v) -}}
{{- else -}}
{{- fail (printf "sandboxPolicy.namespaceSelector.matchExpressions: operator %q is not supported by the namespace label policy (In, NotIn, Exists, DoesNotExist)" .operator) -}}
{{- end -}}
{{- end -}}
{{- if $conds -}}{{- join " && " $conds -}}{{- else -}}true{{- end -}}
{{- end }}

{{/*
koinon.sandboxGuardedLabels is the JSON list of label keys the namespace label policy protects: every
key the sandbox policies' namespace selectors use, plus openshell.ai/managed-by and
openshell.ai/gateway-id (old issue 475).
*/}}
{{- define "koinon.sandboxGuardedLabels" -}}
{{- $sel := .Values.sandboxPolicy.namespaceSelector -}}
{{- $keys := list "openshell.ai/managed-by" "openshell.ai/gateway-id" -}}
{{- range $k, $_ := ($sel.matchLabels | default dict) -}}{{- $keys = append $keys $k -}}{{- end -}}
{{- range ($sel.matchExpressions | default list) -}}{{- $keys = append $keys .key -}}{{- end -}}
{{- $keys | uniq | sortAlpha | toJson -}}
{{- end }}

{{- /*
The release-scoped name of the PlatformAdminRecovery admission policy and its binding (ADR 0110
decision 6): <namespace>-<fullname>-platform-recovery, cut to 63 characters with a hash suffix. Two
releases on one cluster never render the same cluster-scoped name.
*/ -}}
{{- define "koinon.recoveryPolicyName" -}}
{{- $full := printf "%s-%s-platform-recovery" .Release.Namespace (include "koinon.fullname" .) -}}
{{- if gt (len $full) 63 -}}
{{- printf "%s-%s" ($full | trunc 54 | trimSuffix "-") (sha256sum $full | trunc 8) -}}
{{- else -}}
{{- $full -}}
{{- end -}}
{{- end }}

{{/* The runtime class to tier map of the substrate driver, as JSON. The reconciler puts
openshell.runtimeClassName on every sandbox (issue #1245), and the driver gives a command no launcher
policy for a class without a tier. So that cannot go wrong: the class has the tier of
execution.substrate.runtimeClassTiers, or else the one of execution.substrate.defaultTier, or the
render fails. */}}
{{- define "koinon.substrateTierMap" -}}
{{- $m := deepCopy (.Values.execution.substrate.runtimeClassTiers | default dict) -}}
{{- $class := .Values.openshell.runtimeClassName -}}
{{- if and $class (not (hasKey $m $class)) -}}
{{- if .Values.execution.substrate.defaultTier -}}
{{- $_ := set $m $class .Values.execution.substrate.defaultTier -}}
{{- else -}}
{{- fail (printf "execution.substrate.runtimeClassTiers has no entry for openshell.runtimeClassName %q, and execution.substrate.defaultTier is empty: the driver would refuse every command in the sandboxes the reconciler makes. Add runtimeClassTiers.%s or set defaultTier" $class $class) -}}
{{- end -}}
{{- end -}}
{{- toJson $m -}}
{{- end }}

{{/* The runtime class to tier map of the substrate driver as "class=tier" entries, sorted by class. */}}
{{- define "koinon.substrateTiers" -}}
{{- $e := list -}}
{{- range $class, $tier := (include "koinon.substrateTierMap" . | fromJson) -}}
{{- $e = append $e (printf "%s=%s" $class $tier) -}}
{{- end -}}
{{- join "," $e -}}
{{- end }}

{{/*
The gateway recipient key (ADR 0097 decision 12, issue #1151): true when koinon-gateway serves the MCP path
and gateway.mcp.koinon.recipientKey.enabled is on. Empty otherwise.
*/}}
{{- define "koinon.gwKey" -}}
{{- if and (include "koinon.mcpKoinon" .) .Values.gateway.mcp.koinon.recipientKey.enabled }}true{{ end -}}
{{- end }}

{{/* The Secret that holds the wrapped gateway private keys. Only the key-admin Job writes it. */}}
{{- define "koinon.gwKeySecretName" -}}
{{- .Values.gateway.mcp.koinon.recipientKey.secretName | default (printf "%s-gateway-recipient-key" (include "koinon.fullname" .)) -}}
{{- end }}

{{/* The key-admin Job's service account. One name: the subject of the KMS role and of the guard. */}}
{{- define "koinon.gwKeyAdminName" -}}
{{- printf "%s-gateway-key-admin" (include "koinon.fullname" .) -}}
{{- end }}

{{/* Where the gateway pod mounts the wrapped keys, and its projected KMS token. */}}
{{- define "koinon.gwKeyDir" -}}/etc/koinon/gateway-key{{- end }}
{{- define "koinon.gwKeyTokenDir" -}}/var/run/secrets/koinon/gateway-kms{{- end }}

{{/*
The KMS settings of the gateway key (dict root, role: gateway or keyAdmin). The backend, server, region and CA
are secrets.kek.kms's; the wrapping key and the roles are gateway.mcp.koinon.recipientKey.kms's. The key MUST
differ from the KEK's: with the same key the custody boundary is gone, because koinon-api decrypts KEKs.
Empty with the local backend (the gateway key is then held as it is).
*/}}
{{- define "koinon.gwKeyKmsEnv" -}}
{{- $root := .root }}
{{- $rk := $root.Values.gateway.mcp.koinon.recipientKey.kms }}
{{- $kms := $root.Values.secrets.kek.kms }}
{{- if eq (include "koinon.kms" $root) "openbao" }}
{{- $o := $kms.openbao }}
{{- $key := required "gateway.mcp.koinon.recipientKey.kms.openbao.key is required with the openbao backend" $rk.openbao.key }}
{{- if eq $key $o.key }}{{ fail "gateway.mcp.koinon.recipientKey.kms.openbao.key must differ from secrets.kek.kms.openbao.key: koinon-api may decrypt with the KEK's key" }}{{ end }}
- name: KOINON_KEK_BACKEND
  value: openbao
- name: KOINON_OPENBAO_ADDRESS
  value: {{ $o.address | quote }}
- name: KOINON_OPENBAO_TRANSIT_MOUNT
  value: {{ $o.transitMount | default "transit" | quote }}
- name: KOINON_OPENBAO_KEY
  value: {{ $key | quote }}
- name: KOINON_OPENBAO_AUTH_MOUNT
  value: {{ $o.authMount | default "kubernetes" | quote }}
- name: KOINON_OPENBAO_ROLE
  value: {{ index $rk.openbao.roles .role | quote }}
{{- with $o.namespace }}
- name: KOINON_OPENBAO_NAMESPACE
  value: {{ . | quote }}
{{- end }}
{{- with $o.timeout }}
- name: KOINON_OPENBAO_TIMEOUT
  value: {{ . | quote }}
{{- end }}
{{- if $o.caConfigMap }}
- name: KOINON_OPENBAO_CA_FILE
  value: /etc/koinon/openbao-ca/ca.crt
{{- end }}
{{- if eq .role "gateway" }}
- name: KOINON_OPENBAO_TOKEN_FILE
  value: {{ include "koinon.gwKeyTokenDir" $root }}/token
{{- end }}
{{- else if eq (include "koinon.kms" $root) "awskms" }}
{{- $a := $kms.awskms }}
{{- $key := required "gateway.mcp.koinon.recipientKey.kms.awskms.keyId is required with the awskms backend" $rk.awskms.keyId }}
{{- if eq $key $a.keyId }}{{ fail "gateway.mcp.koinon.recipientKey.kms.awskms.keyId must differ from secrets.kek.kms.awskms.keyId: koinon-api may decrypt with the KEK's key" }}{{ end }}
- name: KOINON_KEK_BACKEND
  value: awskms
- name: KOINON_AWSKMS_REGION
  value: {{ $a.region | quote }}
- name: KOINON_AWSKMS_KEY_ID
  value: {{ $key | quote }}
{{- with $a.endpoint }}
- name: KOINON_AWSKMS_ENDPOINT
  value: {{ . | quote }}
{{- end }}
{{- with $a.stsEndpoint }}
- name: KOINON_AWSKMS_STS_ENDPOINT
  value: {{ . | quote }}
{{- end }}
{{- with $a.timeout }}
- name: KOINON_AWSKMS_TIMEOUT
  value: {{ . | quote }}
{{- end }}
{{- if $a.caConfigMap }}
- name: KOINON_AWSKMS_CA_FILE
  value: /etc/koinon/awskms-ca/ca.crt
{{- end }}
{{- end }}
{{- end }}

{{/* The IRSA annotation of the gateway or key-admin ServiceAccount (dict root, role). Empty unless awskms. */}}
{{- define "koinon.gwKeyServiceAccountAnnotations" -}}
{{- if eq (include "koinon.kms" .root) "awskms" }}
{{- with index .root.Values.gateway.mcp.koinon.recipientKey.kms.awskms.roleArns .role }}
eks.amazonaws.com/role-arn: {{ . | quote }}
{{- end }}
{{- end }}
{{- end }}

{{/*
The gateway key-admin Job's pod template (gateway-recipient-key.yaml), as a string: the Job is named by its hash.
dict root, sa, secret (Secret name), rk (values gateway.mcp.koinon.recipientKey).
*/}}
{{- define "koinon.gwKeyAdminPod" -}}
{{- $ := .root -}}
{{- $rk := .rk -}}
metadata:
  labels:
    {{- include "koinon.labels" (list $ "key-admin") | nindent 4 }}
  {{- with $rk.keyAdmin.rotationId }}
  annotations:
    # Changes the hash that names the Job: a new rotationId runs the action once more.
    koinon.dev/rotation-id: {{ . | quote }}
  {{- end }}
spec:
  restartPolicy: Never
  serviceAccountName: {{ .sa }}
  # The token signs in to the KMS (Kubernetes auth) and reaches the Kubernetes API.
  automountServiceAccountToken: true
  enableServiceLinks: false
  {{- with include "koinon.imagePullSecrets" $ | trim }}
  {{- . | nindent 2 }}
  {{- end }}
  securityContext:
    {{- include "koinon.podSecurityContext" $ | nindent 4 }}
  containers:
    - name: gateway-key-admin
      image: {{ include "koinon.image" (list $ $.Values.gateway.mcp.koinon.image) }}
      imagePullPolicy: {{ $.Values.image.pullPolicy }}
      args:
        - keys
        - {{ $rk.keyAdmin.action | default "init" | quote }}
        - {{ printf "--secret=%s" .secret | quote }}
        - {{ printf "--replicas=%d" (int $.Values.gateway.mcp.koinon.replicas) | quote }}
        - {{ printf "--wait-loaded=%s" ($rk.keyAdmin.waitLoaded | default "10m") | quote }}
      env:
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        {{- include "koinon.dbEnv" (list $ "gwkeyadmin") | nindent 8 }}
        {{- include "koinon.gwKeyKmsEnv" (dict "root" $ "role" "keyAdmin") | trim | nindent 8 }}
      securityContext:
        {{- include "koinon.containerSecurityContext" $ | nindent 8 }}
      terminationMessagePolicy: FallbackToLogsOnError
      resources:
        {{- toYaml $rk.keyAdmin.resources | nindent 8 }}
      {{- $mounts := list (include "koinon.dbVolumeMount" $) (include "koinon.kmsVolumeMount" $) | join "\n" | trim }}
      {{- $volumes := list (include "koinon.dbVolume" $) (include "koinon.kmsVolume" $) | join "\n" | trim }}
      {{- with $mounts }}
      volumeMounts:
        {{- . | nindent 8 }}
      {{- end }}
  {{- with $volumes }}
  volumes:
    {{- . | nindent 4 }}
  {{- end }}
{{- end }}

{{/*
The gateway re-seal Job's pod template (gateway-reseal.yaml), as a string: the Job is named by its hash.
It runs under the gateway's service account, because only the gateway identity unwraps the private keys.
dict root, sa (the gateway service account), rk (values gateway.mcp.koinon.recipientKey).
*/}}
{{- define "koinon.gwResealPod" -}}
{{- $ := .root -}}
{{- $rk := .rk -}}
{{- if ne $.Values.postgres.mode "cnpg" }}{{- $_ := required "postgres.external.keys.reseal is required with gateway.mcp.koinon.recipientKey.reseal.enabled" $.Values.postgres.external.keys.reseal }}{{- end }}
metadata:
  labels:
    {{- include "koinon.labels" (list $ "gateway-reseal") | nindent 4 }}
  {{- with $rk.reseal.rotationId }}
  annotations:
    # Changes the hash that names the Job: a new rotationId runs the re-seal once more.
    koinon.dev/rotation-id: {{ . | quote }}
  {{- end }}
spec:
  restartPolicy: Never
  serviceAccountName: {{ .sa }}
  # As the gateway pod: no automatic token. The projected one below signs in to the KMS.
  automountServiceAccountToken: false
  enableServiceLinks: false
  {{- with include "koinon.imagePullSecrets" $ | trim }}
  {{- . | nindent 2 }}
  {{- end }}
  securityContext:
    {{- include "koinon.podSecurityContext" $ | nindent 4 }}
  containers:
    - name: gateway-reseal
      image: {{ include "koinon.image" (list $ $.Values.gateway.mcp.koinon.image) }}
      imagePullPolicy: {{ $.Values.image.pullPolicy }}
      args:
        - keys
        - reseal
        - {{ printf "--recipient-key-dir=%s" (include "koinon.gwKeyDir" $) | quote }}
        {{- if $rk.reseal.dryRun }}
        - "--dry-run"
        {{- end }}
        {{- with $rk.reseal.org }}
        - {{ printf "--org=%s" . | quote }}
        {{- end }}
      env:
        {{- include "koinon.dbEnv" (list $ "reseal") | nindent 8 }}
        {{- include "koinon.gwKeyKmsEnv" (dict "root" $ "role" "gateway") | trim | nindent 8 }}
      securityContext:
        {{- include "koinon.containerSecurityContext" $ | nindent 8 }}
      terminationMessagePolicy: FallbackToLogsOnError
      resources:
        {{- toYaml $rk.reseal.resources | nindent 8 }}
      volumeMounts:
        - name: gateway-key
          mountPath: {{ include "koinon.gwKeyDir" $ }}
          readOnly: true
        {{- if eq (include "koinon.kms" $) "openbao" }}
        - name: gateway-kms-token
          mountPath: {{ include "koinon.gwKeyTokenDir" $ }}
          readOnly: true
        {{- end }}
        {{- include "koinon.dbVolumeMount" $ | nindent 8 }}
        {{- include "koinon.kmsVolumeMount" $ | nindent 8 }}
  volumes:
    - name: gateway-key
      secret:
        secretName: {{ include "koinon.gwKeySecretName" $ }}
        defaultMode: 0444
    {{- if eq (include "koinon.kms" $) "openbao" }}
    - name: gateway-kms-token
      projected:
        sources:
          - serviceAccountToken:
              path: token
              expirationSeconds: 3600
              {{- with $rk.kms.openbao.audience }}
              audience: {{ . | quote }}
              {{- end }}
    {{- end }}
    {{- include "koinon.dbVolume" $ | nindent 4 }}
    {{- include "koinon.kmsVolume" $ | nindent 4 }}
{{- end }}
