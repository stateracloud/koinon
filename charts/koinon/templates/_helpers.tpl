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
{{- include "koinon.componentName" (list (index . 0) (printf "db-%s" (index . 1))) }}
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

{{- define "koinon.configEnv" -}}
- name: KOINON_CONFIG
  value: /etc/koinon/config/config.yaml
{{- end }}

{{- define "koinon.commonEnv" -}}
- name: POD_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
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
A password that is generated once and then read back from the live Secret, so upgrades keep it:
koinon.persistentPassword (list $ "<secret name>" "<key>").
*/}}
{{- define "koinon.persistentPassword" -}}
{{- $ctx := index . 0 -}}
{{- $existing := lookup "v1" "Secret" $ctx.Release.Namespace (index . 1) }}
{{- if and $existing $existing.data (hasKey $existing.data (index . 2)) }}
{{- index $existing.data (index . 2) | b64dec }}
{{- else -}}
{{- randAlphaNum 32 }}
{{- end }}
{{- end }}

{{- define "koinon.gatewayConfigName" -}}
{{- include "koinon.componentName" (list . "gateway-config") }}
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
{{- define "koinon.agentdEnv" -}}
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
{{- if and (not $gatewayURL) (eq .Values.gateway.mode "standalone") }}
{{- $gatewayURL = printf "http://%s:%v" (include "koinon.gatewayHost" .) .Values.gateway.standalone.port }}
{{- end }}
{{- with $gatewayURL }}
- name: KOINON_GATEWAY_URL
  value: {{ . | quote }}
{{- end }}
{{- with .Values.model.models }}
- name: KOINON_DEFAULT_MODEL
  value: {{ (first .).name | quote }}
{{- end }}
- name: KOINON_WORKERS
  value: {{ .Values.agentd.workers | default 8 | quote }}
- name: KOINON_LEASE
  value: {{ .Values.agentd.lease | default "30s" | quote }}
{{- end }}
