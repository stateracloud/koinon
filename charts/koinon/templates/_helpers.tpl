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

{{/* Koinon's own configuration, mounted read-only into api, reconciler and agentd. */}}
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

{{- define "koinon.commonEnv" -}}
- name: KOINON_CONFIG
  value: /etc/koinon/config/config.yaml
- name: POD_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
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

{{/*
Hook annotations of the preflight objects: koinon.preflightHook (list $ "<weight>"). They run
before every other hook; a failed run is kept for inspection and replaced on the next attempt.
*/}}
{{- define "koinon.preflightHook" -}}
helm.sh/hook: pre-install,pre-upgrade
helm.sh/hook-weight: {{ index . 1 | quote }}
helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded
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
{{- $gatewayURL = printf "http://%s.%s.svc:%v" (include "koinon.componentName" (list . "gateway")) .Release.Namespace .Values.gateway.standalone.port }}
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
