# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the gateway recipient key lifecycle (ADR 0097 decision 12, issue #1151, slice 2).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

gk=(--set gateway.mcp.driver=koinon)
gkms=(--set secrets.kek.kms.backend=openbao --set secrets.kek.kms.openbao.address=https://bao.example:8200
	--set secrets.kek.kms.openbao.key=koinon-kek)
gkey_only=(--show-only templates/gateway-recipient-key.yaml)
gmcp_only=(--show-only templates/gateway-koinon-mcp.yaml)
gkeyg=templates/kek-guard.yaml

# Off by default (the agentgateway MCP driver): nothing of the gateway key renders, and no login.
lacks "no gateway key objects by default" 'gateway-recipient-key|gateway-key-admin|gwkeyadmin|gateway-key-guard'
lacks "no gateway key logins in the cluster without the koinon MCP driver" 'koinon_gwkeyadmin|koinon_reseal'
lacks "no gateway key objects with the switch off" 'gateway-key-admin|gateway-key-guard|recipient-key-dir' "${gk[@]}" \
	--set gateway.mcp.koinon.recipientKey.enabled=false

# With the koinon MCP driver: a Job of its own, with a service account of its own, that runs the key command.
has "gateway key ServiceAccount" '^  name: koinon-gateway-key-admin$' "${gkey_only[@]}" "${gk[@]}"
has "gateway key Job runs keys init" '^            - keys$' "${gkey_only[@]}" "${gk[@]}"
has "gateway key Job default action is init" '^            - "init"$' "${gkey_only[@]}" "${gk[@]}"
has "gateway key Job names the Secret" '"--secret=koinon-gateway-recipient-key"' "${gkey_only[@]}" "${gk[@]}"
has "gateway key Job waits for every gateway pod" '"--replicas=2"' "${gkey_only[@]}" "${gk[@]}"
has "gateway key Job uses the gateway image" 'image: ghcr.io/stateracloud/koinon/gateway:' "${gkey_only[@]}" "${gk[@]}"
has "gateway key Job has its own database login" 'name: koinon-db-gwkeyadmin' "${gkey_only[@]}" "${gk[@]}"
has "gateway key Job uses the key-admin database role" 'koinon_gwkeyadmin@' "${gkey_only[@]}" "${gk[@]}"
has "gateway key Job runs the action it is given" '^            - "retire"$' "${gkey_only[@]}" "${gk[@]}" \
	--set gateway.mcp.koinon.recipientKey.keyAdmin.action=retire
gk_out=$(render "${gkey_only[@]}" "${gk[@]}")
grep -Fq -- 'resourceNames: [koinon-gateway-recipient-key]' <<<"$gk_out" || fail "gateway key Role is limited to the key Secret by name"
grep -Fq -- 'verbs: [get, update, delete]' <<<"$gk_out" || fail "gateway key Role reads, updates and deletes the key Secret"
if grep -q 'argocd.argoproj.io/hook' <<<"$gk_out"; then fail "gateway key Job is not an Argo CD hook"; fi
if grep -q 'helm.sh/hook' <<<"$gk_out"; then fail "gateway key Job is not a Helm hook (post-install deadlocks --wait)"; fi
has "gateway key Secret name can be set" 'name: koinon-gateway-key-admin$' "${gkey_only[@]}" "${gk[@]}" \
	--set gateway.mcp.koinon.recipientKey.secretName=mine
has "gateway key Role follows secretName" 'resourceNames: \[mine\]' "${gkey_only[@]}" "${gk[@]}" \
	--set gateway.mcp.koinon.recipientKey.secretName=mine

# The Job is named by its pod template: same inputs, same name; a new action or rotation id, a new Job.
gjob() { render "${gkey_only[@]}" "${gk[@]}" "$@" | grep -A2 '^kind: Job$' | sed -n 's/^  name: //p'; }
j1=$(gjob) j2=$(gjob) j3=$(gjob --set gateway.mcp.koinon.recipientKey.keyAdmin.rotationId=r1)
j4=$(gjob --set gateway.mcp.koinon.recipientKey.keyAdmin.action=rotate --set gateway.mcp.koinon.recipientKey.keyAdmin.rotationId=r1)
[[ $j1 =~ ^koinon-gateway-key-admin-[0-9a-f]{10}$ ]] || fail "gateway key Job name is koinon-gateway-key-admin-<hash>, got '$j1'"
[[ $j1 == "$j2" ]] || fail "gateway key Job name is stable across renders ($j1, $j2)"
[[ $j1 != "$j3" ]] || fail "gateway key Job name changes with rotationId"
[[ $j1 != "$j4" ]] || fail "gateway key Job name changes with the action"
# A rotate Job runs again whenever its pod template changes, so every rotation must name itself.
if render "${gkey_only[@]}" "${gk[@]}" --set gateway.mcp.koinon.recipientKey.keyAdmin.action=rotate >/dev/null 2>&1; then
	fail "action=rotate without a rotationId must fail the render"
fi

# The Job accepts no connections, as the other Jobs.
has "gateway key Job accepts no connections" '^# key-admin accepts no connections' "${gk[@]}" --show-only templates/networkpolicy.yaml

# The gateway pods: the key directory, an optional Secret, and the arg that turns the loader on.
has "gateway loads keys from the directory" '--recipient-key-dir=/etc/koinon/gateway-key' "${gmcp_only[@]}" "${gk[@]}"
has "gateway mounts the key Secret" 'secretName: koinon-gateway-recipient-key' "${gmcp_only[@]}" "${gk[@]}"
has "gateway pod starts without the key Secret" 'optional: true' "${gmcp_only[@]}" "${gk[@]}"
lacks "gateway has no key without the switch" 'gateway-key' "${gmcp_only[@]}" "${gk[@]}" \
	--set gateway.mcp.koinon.recipientKey.enabled=false
lacks "gateway pod still mounts no automatic token" 'automountServiceAccountToken: true' "${gmcp_only[@]}" "${gk[@]}" "${gkms[@]}"
# The Secret is mounted only into the gateway: no other Deployment names it.
other=$(render --show-only templates/deployments.yaml "${gk[@]}" "${gkms[@]}")
if grep -q 'gateway-recipient-key' <<<"$other"; then fail "koinon-api, the reconciler and agentd mount nothing of the gateway key"; fi

# With OpenBao: the gateway wraps with a key of its own and signs in as its own role, with a token it is given.
gw=$(render "${gmcp_only[@]}" "${gk[@]}" "${gkms[@]}")
grep -Fq 'value: "koinon-gateway"' <<<"$gw" || fail "gateway uses the gateway wrapping key, not the KEK's"
if grep -Fq 'value: "koinon-kek"' <<<"$gw"; then fail "gateway pod never names the KEK's transit key"; fi
has "gateway signs in as the gateway role" 'KOINON_OPENBAO_ROLE' "${gmcp_only[@]}" "${gk[@]}" "${gkms[@]}"
has "gateway reads a projected service account token" 'serviceAccountToken:' "${gmcp_only[@]}" "${gk[@]}" "${gkms[@]}"
has "gateway token audience" 'audience: "bao"' "${gmcp_only[@]}" "${gk[@]}" "${gkms[@]}" \
	--set gateway.mcp.koinon.recipientKey.kms.openbao.audience=bao
has "key-admin signs in as the key-admin role" 'value: "koinon-gateway-key-admin"' "${gkey_only[@]}" "${gk[@]}" "${gkms[@]}"
has "key-admin wraps with the gateway wrapping key" 'KOINON_OPENBAO_KEY' "${gkey_only[@]}" "${gk[@]}" "${gkms[@]}"
# The gateway wrapping key must differ from the KEK's: with the same key koinon-api could decrypt it.
if render "${gk[@]}" "${gkms[@]}" --set gateway.mcp.koinon.recipientKey.kms.openbao.key=koinon-kek >/dev/null 2>&1; then
	fail "the gateway wrapping key may not be the KEK's transit key"
fi
# Spec 1 (chart part): koinon-api and the reconciler have no right on the gateway key. The wrapping key
# is only in the gateway and key-admin pods; koinon-api and the reconciler sign in as their own KEK roles.
api=$(render --show-only templates/deployments.yaml "${gk[@]}" "${gkms[@]}")
if grep -Eq 'koinon-gateway-key|gateway-recipient-key|gateway-kms' <<<"$api"; then fail "koinon-api, the reconciler and agentd name nothing of the gateway key or its KMS roles"; fi

# AWS KMS: each service account carries its IRSA role; the key differs from the KEK's.
gaws_key=arn:aws:kms:eu-central-1:111122223333:key/0b1d2e3f-4a5b-4c6d-8e9f-a0b1c2d3e4f5
gaws_key2=arn:aws:kms:eu-central-1:111122223333:key/1c2e3f40-5b6c-4d7e-9fa0-b1c2d3e4f506
gaws=(--set secrets.kek.kms.backend=awskms --set secrets.kek.kms.awskms.region=eu-central-1
	--set "secrets.kek.kms.awskms.keyId=$gaws_key" --set "gateway.mcp.koinon.recipientKey.kms.awskms.keyId=$gaws_key2"
	--set gateway.mcp.koinon.recipientKey.kms.awskms.roleArns.gateway=arn:aws:iam::111122223333:role/koinon-gateway-decrypt
	--set gateway.mcp.koinon.recipientKey.kms.awskms.roleArns.keyAdmin=arn:aws:iam::111122223333:role/koinon-gateway-key-admin-encrypt)
has "gateway ServiceAccount has its IRSA role" 'role-arn: "arn:aws:iam::111122223333:role/koinon-gateway-decrypt"' "${gk[@]}" "${gaws[@]}" "${gmcp_only[@]}"
has "gateway key-admin ServiceAccount has its IRSA role" 'role-arn: "arn:aws:iam::111122223333:role/koinon-gateway-key-admin-encrypt"' "${gk[@]}" "${gaws[@]}" "${gkey_only[@]}"
has "gateway uses the gateway AWS KMS key" "value: \"$gaws_key2\"" "${gk[@]}" "${gaws[@]}" "${gmcp_only[@]}"
if render "${gk[@]}" "${gaws[@]}" --set "gateway.mcp.koinon.recipientKey.kms.awskms.keyId=$gaws_key" >/dev/null 2>&1; then
	fail "the gateway AWS KMS key may not be the KEK's key"
fi
lacks "no gateway IRSA role with OpenBao" 'koinon-gateway-decrypt' "${gk[@]}" "${gkms[@]}"

# The guard: only the key-admin Job's service account may write the Secret. Native policy, never Kyverno.
has "gateway key guard policy" 'name: koinon-koinon-gateway-key-guard$' --show-only "$gkeyg" "${gk[@]}"
has "gateway key guard names the Secret" 'resourceNames: \["koinon-gateway-recipient-key"\]' --show-only "$gkeyg" "${gk[@]}"
has "gateway key guard covers CREATE UPDATE DELETE" 'operations: \[CREATE, UPDATE, DELETE\]' --show-only "$gkeyg" "${gk[@]}"
has "gateway key guard lets only the key-admin write" 'system:serviceaccount:koinon:koinon-gateway-key-admin' --show-only "$gkeyg" "${gk[@]}"
has "gateway key guard follows secretName" 'resourceNames: \["mine"\]' --show-only "$gkeyg" "${gk[@]}" \
	--set gateway.mcp.koinon.recipientKey.secretName=mine
lacks "gateway key guard has its own switch" 'gateway-key-guard' "${gk[@]}" --set gateway.mcp.koinon.recipientKey.guard.enabled=false
has "gateway key guard stays with the KEK guard off" 'gateway-key-guard' "${gk[@]}" --set secrets.kek.guard.enabled=false
lacks "gateway key guard has no Kyverno" 'kyverno' "${gk[@]}"

# The database: two group roles and two logins, each in its own group and in no other.
gpg=(--show-only templates/postgres-cnpg.yaml)
has "cnpg creates the key-admin group role" 'CREATE ROLE koinon_gateway_key_admin NOLOGIN' "${gpg[@]}" "${gk[@]}"
has "cnpg creates the re-seal group role" 'CREATE ROLE koinon_gateway_reseal NOLOGIN' "${gpg[@]}" "${gk[@]}"
has "key-admin login is in its group role only" 'inRoles: \[koinon_gateway_key_admin\]' "${gpg[@]}" "${gk[@]}"
has "re-seal login is in its group role only" 'inRoles: \[koinon_gateway_reseal\]' "${gpg[@]}" "${gk[@]}"
lacks "gateway key logins are not in koinon_app" 'inRoles: \[koinon_app.*(gwkeyadmin|reseal)' "${gpg[@]}" "${gk[@]}"
lacks "no gateway key logins without the switch" 'name: koinon_gwkeyadmin|name: koinon_reseal' "${gpg[@]}" "${gk[@]}" \
	--set gateway.mcp.koinon.recipientKey.enabled=false
has "generated passwords for the gateway key logins" 'koinon-db-gwkeyadmin' --show-only templates/ensure-secrets.yaml "${gk[@]}"
has "external mode reads the key-admin login URI" 'key: gwkeyadmin-uri' "${gkey_only[@]}" "${gk[@]}" \
	--set postgres.mode=external --set postgres.external.secretName=pg

# Custody text: local says koinon-api can read the key; a KMS says only the gateway can unwrap.
# NOTES are printed by install, not by template.
gnotes() {
	local out rc=0
	out=$("$HELM" install koinon "$CHART" -n koinon --dry-run=client --set openshell.insecure=true --set "$ADDRESS" "$@" 2>&1) || rc=$?
	if [[ $rc -ne 0 ]]; then
		fail "helm install --dry-run=client for the gateway key NOTES failed: $(head -1 <<<"$out")"
		return 0
	fi
	printf '%s\n' "$out"
}
notes_local=$(gnotes "${gk[@]}")
grep -Fq 'custody local' <<<"$notes_local" || fail "NOTES report custody local with the local backend"
if grep -Fq 'koinon-api cannot' <<<"$notes_local"; then fail "NOTES never claim koinon-api cannot decrypt on the local backend"; fi
notes_kms=$(gnotes "${gk[@]}" "${gkms[@]}")
grep -Fq 'custody kms' <<<"$notes_kms" || fail "NOTES report custody kms with a KMS"

# koinon-api seals MCP credentials to the gateway key (envelope v2) only when the koinon MCP driver is on. The
# agentgateway driver reads plaintext files, so it keeps v1 (ADR 0097 decision 12, 12.5 step 0).
env_is KOINON_GATEWAY_MCP_SEAL_V2 '"true"' "${gk[@]}"
lacks "koinon-api seals v1 with the agentgateway MCP driver" 'KOINON_GATEWAY_MCP_SEAL_V2' --show-only templates/deployments.yaml
