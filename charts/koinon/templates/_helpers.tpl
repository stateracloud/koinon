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
  value: {{ printf "http://%s:%s/mcp" (include "koinon.gatewayHost" .) (include "koinon.gatewayPort" .) | quote }}
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

{{/*
The OpenShell gateway's host:port. The reconciler and agentd refuse to start without it, so the
chart refuses to render without it (the install would otherwise end in crash-looping pods).
*/}}
{{- define "koinon.openshellAddress" -}}
{{- required "openshell.address is required: set it to the OpenShell gateway's host:port, for example openshell.openshell.svc:8080" .Values.openshell.address -}}
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
{{- if not .Values.api.otlpToken.existingSecret -}}
{{- $specs = append $specs (dict "name" (include "koinon.otlpTokenName" .) "type" "Opaque" "kind" "bearer-token" "labels" (fromYaml (include "koinon.generatedLabels" (list . "api")))) -}}
{{- end -}}
{{- if not .Values.api.otlpTracesToken.existingSecret -}}
{{- $specs = append $specs (dict "name" (include "koinon.otlpTracesTokenName" .) "type" "Opaque" "kind" "bearer-token" "labels" (fromYaml (include "koinon.generatedLabels" (list . "api")))) -}}
{{- end -}}
{{- if not .Values.installMark.existingSecret -}}
{{- $specs = append $specs (dict "name" (include "koinon.installMarkName" .) "type" "Opaque" "kind" "bearer-token" "labels" (fromYaml (include "koinon.generatedLabels" (list . "reconciler")))) -}}
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
the pod that role's web identity. Empty otherwise.
*/}}
{{- define "koinon.kmsServiceAccountAnnotations" -}}
{{- if eq (include "koinon.kms" .root) "awskms" }}
{{- with index .root.Values.secrets.kek.kms.awskms.roleArns .role }}
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

{{- define "koinon.validate" -}}
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
- name: KOINON_OPENSHELL_ADDRESS
  value: {{ include "koinon.openshellAddress" . | quote }}
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
- name: KOINON_DRAIN_DEADLINE
  value: {{ printf "%ds" (int (.Values.agentd.drainDeadlineSeconds | default 300)) | quote }}
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
