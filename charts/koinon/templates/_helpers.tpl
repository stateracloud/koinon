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
The image pull Secret names, as JSON: global.imagePullSecrets and the deprecated top-level
imagePullSecrets, merged and de-duplicated. Each entry is {name: x} or a plain "x".
*/}}
{{- define "koinon.pullSecretNames" -}}
{{- $names := list -}}
{{- $g := .Values.global | default dict -}}
{{- range concat ($g.imagePullSecrets | default list) (.Values.imagePullSecrets | default list) -}}
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
sandboxPolicy.supervisorImages, or when that is empty OpenShell 0.1.5's own default, rewritten by
koinon.imageRef so that a mirror needs no extra setting. An explicit list is used as written.
*/}}
{{- define "koinon.supervisorImages" -}}
{{- if .Values.sandboxPolicy.supervisorImages -}}
{{- .Values.sandboxPolicy.supervisorImages | toJson -}}
{{- else -}}
{{- list (include "koinon.imageRef" (list . "ghcr.io/nvidia/openshell/supervisor:0.1.5")) | toJson -}}
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
# Approved calls to remote MCP servers go through the gateway (ADR 0057): it holds the credentials.
- name: KOINON_GATEWAY_MCP_URL
  value: {{ printf "http://%s:%s/mcp" (include "koinon.gatewayHost" .) (include "koinon.gatewayPort" .) | quote }}
- name: KOINON_KEK_ACTIVE_PERIOD
  value: {{ .Values.secrets.kek.activePeriod | quote }}
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
{{- range $role := list "migrate" "api" "reconciler" "agentd" -}}
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
{{- $specs | toJson -}}
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
  value: {{ if .root.Values.secrets.kek.existingSecret }}"false"{{ else }}"true"{{ end }}
{{- end }}
{{- end }}

{{- define "koinon.validate" -}}
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
{{- if and .Values.flavorPrepull.enabled (not .Values.flavorPrepull.nodeSelector) }}
{{- fail "flavorPrepull.nodeSelector is required when flavorPrepull.enabled is true: select the Kata nodes, or the images are pulled onto every node" }}
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
{{- if and (not .Values.openshell.clientTLSSecret) (not .Values.openshell.insecure) }}
{{- fail "openshell.clientTLSSecret is required: Koinon dials the OpenShell gateway (it controls every sandbox) over mTLS. Set openshell.clientTLSSecret to a Secret with the gateway's client certificate (tls.crt, tls.key, ca.crt), or set openshell.insecure=true only if a service mesh provides mTLS between Koinon and the gateway" }}
{{- end }}
{{- if and .Values.openshell.clientTLSSecret .Values.openshell.insecure }}
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
{{- /* The uncommitted work of a stuck sandbox is posted here to be sealed; agentd holds no KEK (ADR 0054). */}}
- name: KOINON_SAVED_WORK_KEY_FILE
  value: /secrets/saved-work/key
- name: KOINON_SAVED_WORK_URL
  value: {{ printf "http://%s.%s.svc.%s:%v" (include "koinon.componentName" (list . "api-saved-work")) .Release.Namespace (.Values.clusterDomain | default "cluster.local") .Values.api.savedWorkPort | quote }}
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: KOINON_OPENSHELL_ADDRESS
  value: {{ .Values.openshell.address | quote }}
{{- if .Values.openshell.clientTLSSecret }}
- name: KOINON_OPENSHELL_TLS_DIR
  value: /etc/koinon/openshell-tls
{{- end }}
{{- $gatewayURL := .Values.agentd.gatewayURL }}
{{- if not $gatewayURL }}
{{- $gatewayURL = printf "http://%s:%s" (include "koinon.gatewayHost" .) (include "koinon.gatewayPort" .) }}
{{- end }}
{{- with $gatewayURL }}
- name: KOINON_GATEWAY_URL
  value: {{ . | quote }}
{{- end }}
- name: KOINON_WORKERS
  value: {{ .Values.agentd.workers | default 8 | quote }}
- name: KOINON_LEASE
  value: {{ .Values.agentd.lease | default "30s" | quote }}
{{- if .Values.checkpoints.enabled }}
{{- /* Home checkpoints: agentd is the writer, the only holder of the S3 credentials, and holds no KEK: koinon-api hands it each repository password (ADR 0032, 0054). */}}
- name: KOINON_CHECKPOINTS
  value: "true"
- name: KOINON_CHECKPOINTS_INTERVAL
  value: {{ .Values.checkpoints.interval | quote }}
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
