# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: GitOps-safe renders, ensure-secrets, Argo CD hooks, OTLP token.
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# GitOps-safe renders (old issue 98). (a) Two renders of every value set are byte-identical.
for f in "${value_files[@]}"; do
	name=$(basename "$f" .yaml)
	"$HELM" template koinon "$CHART" --namespace koinon -f "$f" $(insecure_for "$f") >"$out/$name.again"
	cmp -s "$out/$name.yaml" "$out/$name.again" || fail "two renders of $name differ"
	rm -f "$out/$name.again"
done
# (b) No lookup remains: a render must not depend on the cluster.
if grep -rnE '\blookup\b' "$CHART/templates"; then fail "a template uses lookup"; fi
# kinds_named <kind> <helm args...>: the names of the objects of that kind in the rendering.
kinds_named() {
	local kind=$1
	shift
	render "$@" | awk -v kind="$kind" '
		/^---$/ { if (k == kind && n != "") print n; k = ""; n = ""; meta = 0; next }
		/^kind: / { k = $2 }
		/^metadata:/ { meta = 1; next }
		meta && /^  name: / && n == "" { n = $2 }
		/^[a-z]/ && !/^metadata:/ { meta = 0 }
		END { if (k == kind && n != "") print n }'
}
# (c) cnpg mode: the hook Job exists and no Secret is rendered for the roles or the bootstrap admin.
has "ensure-secrets hook Job in cnpg mode" '^            - ensure-secrets$' --show-only templates/ensure-secrets.yaml
has "ensure-secrets is a pre-install,pre-upgrade hook" 'helm.sh/hook: pre-install,pre-upgrade' --show-only templates/ensure-secrets.yaml
# Argo CD's label tracking would claim (and prune) Secrets that carry the instance label.
lacks "generated Secrets carry no app.kubernetes.io/instance label" '--secrets=.*app\.kubernetes\.io/instance' --show-only templates/ensure-secrets.yaml --set postgres.mode=cnpg
secrets=$(kinds_named Secret | tr '\n' ' ')
[[ $secrets == *koinon-db-* || $secrets == *koinon-bootstrap-admin* ]] && fail "cnpg mode renders Secrets $secrets"
for want in koinon-db-migrate koinon-db-api koinon-db-reconciler koinon-db-agentd koinon-bootstrap-admin; do
	has "the hook Job ensures $want" "$want" --show-only templates/ensure-secrets.yaml
done
# agentgateway v1.7 always encrypts MCP sessions: standalone mode gets SESSION_KEY from a generated
# Secret (ensure-secrets), never from a template value; kubernetes mode has no standalone gateway.
has "the hook ensures the gateway session key" 'aes256-key' --show-only templates/ensure-secrets.yaml
has "the gateway reads SESSION_KEY from the generated Secret" 'name: SESSION_KEY' --show-only templates/gateway-standalone.yaml
has "SESSION_KEY comes from the Secret's key" 'name: koinon-gateway-session-key$' --show-only templates/gateway-standalone.yaml
lacks "the hook leaves the session key out in kubernetes mode" 'gateway-session-key' --set gateway.mode=kubernetes --show-only templates/ensure-secrets.yaml
lacks "no SESSION_KEY in kubernetes mode" 'SESSION_KEY' --set gateway.mode=kubernetes
has "an existing session key Secret is referenced" 'name: mine-session$' --set gateway.standalone.sessionKey.existingSecret=mine-session --show-only templates/gateway-standalone.yaml
lacks "an existing session key Secret is not ensured" 'gateway-session-key' --set gateway.standalone.sessionKey.existingSecret=mine-session --show-only templates/ensure-secrets.yaml
# (d) With every Secret supplied there is no hook Job, and the given names are referenced.
own=(--set gateway.standalone.sessionKey.existingSecret=mine-session --set bootstrapAdmin.existingSecret=mine-admin
	--set api.otlpToken.existingSecret=mine-otlp --set installMark.existingSecret=mine-mark
	--set postgres.cnpg.existingSecrets.migrate=mine-migrate --set postgres.cnpg.existingSecrets.api=mine-api
	--set postgres.cnpg.existingSecrets.reconciler=mine-reconciler --set postgres.cnpg.existingSecrets.agentd=mine-agentd)
if render "${own[@]}" --show-only templates/ensure-secrets.yaml >/dev/null 2>&1; then
	fail "the ensure-secrets hook renders although every Secret is supplied"
fi
lacks "no ensure-secrets Job with existing Secrets" 'ensure-secrets' "${own[@]}"
for name in mine-migrate mine-api mine-reconciler mine-agentd; do
	has "$name is referenced" "name: $name\$" "${own[@]}"
done
lacks "the generated role Secret names are unused with existing Secrets" 'koinon-db-' "${own[@]}"
has "the Cluster's initdb takes the given migrate Secret" 'name: mine-migrate$' "${own[@]}" --show-only templates/postgres-cnpg.yaml
# Argo CD (old issue 118, ADR 0060): the cnpg install Job is a Sync hook in wave 1 recreated each sync, and what
# must follow it (the Deployments, the standalone gateway, which needs the reconciler's ConfigMap) is in wave 2.
mig=$(render --show-only templates/migrate-job.yaml)
for want in 'argocd.argoproj.io/hook: Sync' 'argocd.argoproj.io/hook-delete-policy: BeforeHookCreation' 'argocd.argoproj.io/sync-wave: "1"'; do
	grep -qF -- "$want" <<<"$mig" || fail "the cnpg migration Job lacks '$want'"
done
lacks "the external-mode migration hook has no Argo annotations" 'argocd.argoproj.io' --set postgres.mode=external --set postgres.external.secretName=db --show-only templates/migrate-job.yaml
n=$(render --show-only templates/deployments.yaml | grep -c 'argocd.argoproj.io/sync-wave: "2"' || true)
[ "$n" = 3 ] || fail "$n of the 3 koinon Deployments are in sync wave 2"
n=$(render --show-only templates/gateway-standalone.yaml | grep -c 'argocd.argoproj.io/sync-wave: "2"' || true)
[ "$n" = 2 ] || fail "$n of the gateway's PVC and Deployment are in sync wave 2, want 2"
has "the migration stays a plain Job on install" 'name: koinon-migrate-install$' --show-only templates/migrate-job.yaml
has "the migration Job reads the given migrate Secret" 'name: mine-migrate$' "${own[@]}" --show-only templates/migrate-job.yaml
# One role supplied: the hook ensures the rest only.
lacks "the hook leaves out a supplied role" 'koinon-db-api' --set postgres.cnpg.existingSecrets.api=mine-api --show-only templates/ensure-secrets.yaml
has "the hook still ensures the others" 'koinon-db-migrate' --set postgres.cnpg.existingSecrets.api=mine-api --show-only templates/ensure-secrets.yaml
# External Postgres with a given admin Secret: no Job either.
if render --set postgres.mode=external --set postgres.external.secretName=dsns --set bootstrapAdmin.existingSecret=mine-admin \
	--set gateway.standalone.sessionKey.existingSecret=mine-session --set api.otlpToken.existingSecret=mine-otlp --set installMark.existingSecret=mine-mark \
	--show-only templates/ensure-secrets.yaml >/dev/null 2>&1; then
	fail "the ensure-secrets hook renders in external mode with an existing admin Secret"
fi
has "external mode still ensures the bootstrap admin" 'koinon-bootstrap-admin' --set postgres.mode=external \
	--set postgres.external.secretName=dsns --show-only templates/ensure-secrets.yaml
# The hook Job reads exactly the Secrets it ensures and creates; it updates and deletes nothing.
lacks "ensure-secrets can update or delete" 'verbs: \[(update|delete|patch)' --show-only templates/ensure-secrets.yaml
# The internal OTLP listener's bearer token (ADR 0074, old issue 52): generated by the hook Job (never a template
# value), mounted into koinon-api, koinon-agentd and the standalone gateway, referenced by name from the
# reconciler's config, and never written into a ConfigMap.
has "the hook ensures the OTLP token as a bearer token" 'bearer-token' --show-only templates/ensure-secrets.yaml
has "the hook ensures koinon-otlp-token" 'koinon-otlp-token' --show-only templates/ensure-secrets.yaml
for mode in standalone kubernetes; do
	m=(--set gateway.mode=$mode)
	n=$(render "${m[@]}" --show-only templates/deployments.yaml | grep -c 'secretName: koinon-otlp-token$' || true)
	[ "$n" = 2 ] || fail "the OTLP token Secret is mounted by $n of koinon-api and koinon-agentd in $mode mode, want 2 (not the reconciler)"
	n=$(render "${m[@]}" --show-only templates/deployments.yaml | grep -c 'mountPath: /secrets/otlp$' || true)
	[ "$n" = 2 ] || fail "$n OTLP token mounts in $mode mode, want 2"
	env_is KOINON_OTLP_TOKEN_DIR '"/secrets/otlp"' "${m[@]}"
	has "agentd gets the OTLP endpoint in $mode mode" 'value: "http://koinon-api\.koinon\.svc\.cluster\.local:4318"' "${m[@]}" --show-only templates/deployments.yaml
	has "agentd gets the OTLP token file in $mode mode" 'value: "/secrets/otlp/token"' "${m[@]}" --show-only templates/deployments.yaml
	lacks "the OTLP token never reaches a ConfigMap data value in $mode mode" '[0-9a-f]{64}' "${m[@]}" --show-only templates/config.yaml
	# The otlp ingress rule lists two senders: the gateway (or its proxy) and agentd.
	n=$(render "${m[@]}" --show-only templates/networkpolicy.yaml | grep -A14 -E '^        - port: otlp$' | grep -c 'app.kubernetes.io/component: agentd' || true)
	[ "$n" = 1 ] || fail "the OTLP ingress admits agentd $n times in $mode mode, want 1"
done
# The token file is mode 0440 and group-readable by the pod's fsGroup, so the non-root gateway, api and agentd read it.
has "the token volume is group-readable (0440)" 'defaultMode: 0440' --show-only templates/gateway-standalone.yaml
has "the gateway pod's fsGroup is set, to read the 0440 token" 'fsGroup: 65532' --show-only templates/gateway-standalone.yaml
has "the standalone gateway mounts the token Secret" 'secretName: koinon-otlp-token$' --set gateway.mode=standalone --show-only templates/gateway-standalone.yaml
has "the standalone gateway sees the token at /secrets/otlp" 'mountPath: /secrets/otlp$' --set gateway.mode=standalone --show-only templates/gateway-standalone.yaml
has "the reconciler config names the token file (standalone)" 'accessLogOtlpTokenFile: /secrets/otlp/token$' --set gateway.mode=standalone --show-only templates/config.yaml
lacks "the standalone config names no token Secret" 'accessLogOtlpTokenSecret' --set gateway.mode=standalone --show-only templates/config.yaml
has "the reconciler config names the token Secret (kubernetes)" 'accessLogOtlpTokenSecret: koinon-otlp-token$' "${k8s[@]}" --show-only templates/config.yaml
lacks "the kubernetes config names no token file" 'accessLogOtlpTokenFile' "${k8s[@]}" --show-only templates/config.yaml
lacks "no gateway pod mounts the token in kubernetes mode (the controller reads it)" 'secretName: koinon-otlp-token' "${k8s[@]}" --show-only templates/gateway-kubernetes.yaml
has "an existing OTLP token Secret is mounted" 'secretName: mine-otlp$' --set api.otlpToken.existingSecret=mine-otlp --show-only templates/deployments.yaml
has "an existing OTLP token Secret reaches the config" 'accessLogOtlpTokenSecret: mine-otlp$' "${k8s[@]}" --set api.otlpToken.existingSecret=mine-otlp --show-only templates/config.yaml
lacks "an existing OTLP token Secret is not ensured" 'koinon-otlp-token' --set api.otlpToken.existingSecret=mine-otlp --show-only templates/ensure-secrets.yaml
# (e) Standalone mode renders no gateway config ConfigMap (the reconciler creates it); the Deployment mounts it.
cms=$(kinds_named ConfigMap --set gateway.mode=standalone | tr '\n' ' ')
[[ $cms == *gateway-config* ]] && fail "standalone mode renders the gateway ConfigMap ($cms)"
has "the gateway mounts its config by name" 'name: koinon-gateway-config$' --set gateway.mode=standalone --show-only templates/gateway-standalone.yaml
has "the reconciler may create the config and read the gateway Deployment" 'resourceNames: \[koinon-gateway\]' --set gateway.mode=standalone --show-only templates/rbac.yaml
has "the standalone reconciler may read the gateway pods' logs (agentgateway's reload errors, ADR 0050)" 'resources: \[pods/log\]' --set gateway.mode=standalone --show-only templates/rbac.yaml
has "the standalone reconciler may list the gateway pods" 'resources: \[pods\]' --set gateway.mode=standalone --show-only templates/rbac.yaml
lacks "the kubernetes-mode reconciler reads no pod logs" 'pods/log' "${k8s[@]}" --show-only templates/rbac.yaml
has "the reconciler is told the gateway Deployment" 'deployment: koinon-gateway$' --set gateway.mode=standalone --show-only templates/config.yaml

# The install mark (issue 461): a random value the hook Job generates once, in a Secret and not in Postgres.
# koinon-reconciler and koinon-agentd read it from the environment, and since ADR 0110 so does koinon-api: it
# stamps the tokens and sessions it issues with the hash and refuses those of another install (a restored copy).
has "the hook ensures koinon-install-mark" 'koinon-install-mark' --show-only templates/ensure-secrets.yaml
n=$(render --show-only templates/deployments.yaml | grep -c 'name: KOINON_INSTALL_MARK$' || true)
[ "$n" = 3 ] || fail "$n components get KOINON_INSTALL_MARK, want 3 (api, reconciler and agentd)"
n=$(render --show-only templates/deployments.yaml | grep -A4 'name: KOINON_INSTALL_MARK$' | grep -c 'name: koinon-install-mark$' || true)
[ "$n" = 3 ] || fail "$n install mark references read koinon-install-mark, want 3"
lacks "the install mark never reaches a ConfigMap" 'install-mark' --show-only templates/config.yaml
lacks "an existing install mark Secret is not ensured" 'koinon-install-mark' --set installMark.existingSecret=mine-mark --show-only templates/ensure-secrets.yaml
has "an existing install mark Secret is read" 'name: mine-mark$' --set installMark.existingSecret=mine-mark --show-only templates/deployments.yaml
