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

{{/* Koinon image reference: koinon.image (list $ .Values.api.image). */}}
{{- define "koinon.image" -}}
{{- $ctx := index . 0 -}}
{{- $img := index . 1 }}
{{- $tag := $img.tag | default $ctx.Values.image.tag | default $ctx.Chart.AppVersion }}
{{- $ref := printf "%s/%s:%s" $ctx.Values.image.registry $img.repository $tag }}
{{- if $img.digest }}{{ $ref = printf "%s@%s" $ref $img.digest }}{{ end }}
{{- $ref }}
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

{{- define "koinon.imagePullSecrets" -}}
{{- with .Values.imagePullSecrets -}}
imagePullSecrets:
  {{- toYaml . | nindent 2 }}
{{- end }}
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
The URL browsers reach Koinon at: publicUrl, else derived from the ingress, else empty (which
koinon-api takes as https).
*/}}
{{- define "koinon.publicUrl" -}}
{{- if .Values.publicUrl -}}
{{- trimSuffix "/" .Values.publicUrl -}}
{{- else if .Values.ingress.enabled -}}
{{- printf "%s://%s" (ternary "https" "http" (not (empty .Values.ingress.tlsSecretName))) .Values.ingress.host -}}
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
Labels of a Secret the ensure-secrets Job creates. The Job's objects are not owned by the release,
so they carry no chart version or managed-by label that would go stale: koinon.generatedLabels (list $ "api").
*/}}
{{- define "koinon.generatedLabels" -}}
{{ include "koinon.selectorLabels" . }}
app.kubernetes.io/part-of: koinon
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
{{- $specs | toJson -}}
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
{{- if and .Values.oidc.enabled (not .Values.oidc.redirectUrl) (not (include "koinon.publicUrl" .)) }}
{{- fail "oidc.enabled needs oidc.redirectUrl, publicUrl or an ingress to build the redirect URL from" }}
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
{{- end }}
