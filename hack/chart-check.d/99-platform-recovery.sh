# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: PlatformAdminRecovery, the request object that regains platform admin access without
# kubectl exec (issue #512, ADR 0110 decision 6). Sourced in order by hack/chart-check.sh.

pr=templates/platform-recovery.yaml
crd=charts/koinon/crds/platformadminrecoveries.yaml

# The CRD ships in crds/ (never templated), with a status subresource and the closed action enum.
[[ -f "$CHART/crds/platformadminrecoveries.yaml" ]] || fail "the PlatformAdminRecovery CRD is in crds/"
hit 'subresources:' "$CHART/crds/platformadminrecoveries.yaml" && hit 'status: {}' "$CHART/crds/platformadminrecoveries.yaml" || fail "the CRD has a status subresource"
hit 'enum: \[mint-admin-token, reset-password\]' "$CHART/crds/platformadminrecoveries.yaml" || fail "the CRD action is a closed enum"
hit "pattern: '\^\[A-Za-z0-9_-\]{16,64}\$'" "$CHART/crds/platformadminrecoveries.yaml" || fail "the CRD checks the nonce"

# A native ValidatingAdmissionPolicy (never Kyverno), failing closed, bound to the release namespace with Deny.
has "recovery policy is a native VAP" 'kind: ValidatingAdmissionPolicy$' --show-only "$pr"
has "recovery policy fails closed" 'failurePolicy: Fail' --show-only "$pr"
has "recovery binding denies" 'validationActions: \["Deny"\]' --show-only "$pr"
has "recovery binding selects the release namespace" 'kubernetes.io/metadata.name: "koinon"' --show-only "$pr"
has "recovery policy pins the requester to the authenticated user" 'object.spec.requester == request.userInfo.username' --show-only "$pr"
has "recovery policy pins the groups" 'object.spec.requesterGroups == request.userInfo.groups' --show-only "$pr"
has "recovery policy refuses a changed spec" 'object.spec == oldObject.spec' --show-only "$pr"
has "recovery policy caps the ttl at 1h" "duration\(object.spec.ttl\) <= duration\('1h'\)" --show-only "$pr"
has "recovery policy requires the recipient key" 'has\(object.spec.recipientPublicKey\)' --show-only "$pr"
lacks "recovery has no Kyverno" 'kyverno' --show-only "$pr"
# Release-scoped, cluster-scoped names.
has "recovery policy name carries the namespace and release" 'name: koinon-koinon-platform-recovery$' --show-only "$pr"
has "recovery policy name is release-scoped in another namespace" 'name: other-ns-koinon-platform-recovery$' --show-only "$pr" --namespace other-ns

# The requester Role exists and is bound to nobody by default.
has "recovery requester Role" 'kind: Role$' --show-only "$pr"
has "recovery requester may create the object" 'verbs: \[create, get, list, watch\]' --show-only "$pr"
rb_count() { render --show-only "$pr" "$@" | grep -c -E -- '^  name: koinon-platform-recovery$' || true; }
[[ $(rb_count) == 1 ]] || fail "the recovery Role is bound to nobody by default, got $(rb_count) mentions of its name"
rs=(--set 'recovery.subjects[0].kind=User' --set 'recovery.subjects[0].name=alice' --set 'recovery.subjects[0].apiGroup=rbac.authorization.k8s.io')
has "recovery.subjects binds the Role" 'name: alice$' --show-only "$pr" "${rs[@]}"
if render --set 'recovery.subjects[0].kind=Robot' --set 'recovery.subjects[0].name=x' >/dev/null 2>&1; then fail "a recovery subject kind outside User, Group, ServiceAccount fails the render"; fi

# The reconciler: the objects and their status, plus a get on this release's policy and binding only.
has "reconciler may update the status" 'platformadminrecoveries/status' --show-only "$pr"
has "reconciler gets the policy by name" 'resourceNames: \[koinon-koinon-platform-recovery\]' --show-only "$pr"
lacks "reconciler gets no Secret for recovery" 'secrets' --show-only "$pr"
lacks "reconciler makes no Role for recovery" 'roles' --show-only "$pr"

# The reconciler is told the names to check, so it refuses requests when the policy is not in force.
has "reconciler config names the recovery policy" 'policy: "koinon-koinon-platform-recovery"' --show-only templates/config.yaml
has "reconciler config names the recovery binding" 'binding: "koinon-koinon-platform-recovery"' --show-only templates/config.yaml

# Off: nothing renders and the reconciler runs no handler.
lacks "recovery.enabled=false renders no policy" 'platform-recovery' --set recovery.enabled=false
lacks "recovery.enabled=false leaves the reconciler config without recovery" 'recovery:' --show-only templates/config.yaml --set recovery.enabled=false
