#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The Substrate parts of the kind e2e (issue #1091, epic #1082). test/e2e/run.sh calls it with DRIVER=substrate; kind/up.sh
# DRIVER=substrate creates the cluster without OpenShell first. Every kubectl and helm call names --context kind-$CLUSTER.
#
#   substrate.sh harness   build the substrate overlay of the harness image (make harness-substrate-opencode's
#                          Dockerfile: the guest front and the launcher), push it to the local registry as
#                          <repo>/sandbox-substrate:e2e and print its digest reference. Needs <repo>/sandbox:e2e in
#                          the local daemon (images.sh build leaves it there). Substrate runs only an image pinned by
#                          digest (#1192), so the e2e values carry this digest.
#   substrate.sh install   install Substrate with `koinon-api substrate install --platform kind`, gvisor class only
#                          (a standard runner has no KVM, ADR 0119 D10), from the pinned commit of a clone.
#
#   CLUSTER=koinon-e1  OUT=<dir for the binary, the kubeconfig and the digest>
#   SUBSTRATE_SOURCE=<clone of github.com/agent-substrate/substrate that holds the pinned commit>. Unset: a shallow
#     fetch of the pinned commit into $SUBSTRATE_CACHE (default ~/.cache/koinon/substrate).
#   GVISOR_MIRROR=<base url>  passed as --gvisor-mirror when the installer host cannot reach storage.googleapis.com
#     (docs/dev/substrate-install.md, "Mirror and air-gapped installs").

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

mode=${1:-}
case $mode in harness | install | ca) ;; *) echo "usage: substrate.sh harness|install|ca" >&2; exit 2 ;; esac

CLUSTER=${CLUSTER:-koinon-e1}
CTX="kind-${CLUSTER}"
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../../.." && pwd)"
OUT=${OUT:-$(mktemp -d "${TMPDIR:-/tmp}/substrate.XXXXXX")}
mkdir -p "$OUT/bin"
REPO=$("$here/imagerepo.sh")
# kind create cluster names the bucket of its own object store; the installer needs it to exist (docs/dev/substrate-install.md).
BUCKET=ate-snapshots

# Temp directories of this run, removed on exit (a RETURN trap does not run when the script exits).
TMP_DIRS=()
cleanup() { if ((${#TMP_DIRS[@]})); then rm -rf "${TMP_DIRS[@]}"; fi; }
trap cleanup EXIT

step() { echo "== $(date -u +%H:%M:%SZ) $*" >&2; }

harness() {
  local arch src
  arch=$(go env GOARCH)
  src="$REPO/sandbox:e2e"
  docker image inspect "$src" >/dev/null 2>&1 || { echo "substrate.sh: $src is not in the local daemon; run kind/images.sh build first" >&2; exit 1; }
  step "guest front and launcher (linux/$arch)"
  (cd "$root" && make guest-launcher >&2)
  step "substrate overlay of the harness image: $REPO/sandbox-substrate:e2e"
  # The same build as `make harness-substrate-<name>`: a docker-container builder of its own, the harness image as an OCI
  # layout in a temporary directory (outside $OUT, which CI uploads) that the script removes on exit.
  "$root/hack/harness-substrate-build.sh" "$src" "$REPO/sandbox-substrate:e2e" "linux/$arch"
  docker push -q "$REPO/sandbox-substrate:e2e" >/dev/null
  # The digest of what the registry now holds under the tag. The node pulls it from the registry, so the registry's
  # digest is the one the chart must pin (a local RepoDigests entry can differ after a push through another manifest type).
  local digest
  digest=$(docker buildx imagetools inspect "$REPO/sandbox-substrate:e2e" --format '{{.Manifest.Digest}}' 2>/dev/null || true)
  if [[ -z $digest ]]; then
    digest=$(docker inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$REPO/sandbox-substrate:e2e" | sed -n "s#^.*@##p" | head -n1)
  fi
  [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "substrate.sh: no digest for $REPO/sandbox-substrate:e2e (got '$digest')" >&2; exit 1; }
  printf '%s\n' "$digest" >"$OUT/harness-substrate.digest"
  echo "$digest"
}

# source_clone: a directory that holds the pinned commit. The installer exports that commit with git archive and trusts
# nothing else in the directory.
source_clone() {
  local commit dir
  commit=$(sed -n 's/^[[:space:]]*SubstratePinnedCommit[[:space:]]*=[[:space:]]*"\([0-9a-f]\{40\}\)".*/\1/p' "$root/internal/preflight/substratepin.go")
  [[ -n $commit ]] || { echo "substrate.sh: cannot read the pinned commit from internal/preflight/substratepin.go" >&2; exit 1; }
  if [[ -n ${SUBSTRATE_SOURCE:-} ]]; then
    echo "$SUBSTRATE_SOURCE"
    return
  fi
  dir=${SUBSTRATE_CACHE:-$HOME/.cache/koinon/substrate}
  if [[ ! -d $dir/.git ]]; then
    mkdir -p "$dir"
    git -C "$dir" init -q
    git -C "$dir" remote add origin https://github.com/agent-substrate/substrate.git
  fi
  if ! git -C "$dir" cat-file -e "$commit^{commit}" 2>/dev/null; then
    git -C "$dir" fetch -q --depth 1 origin "$commit" >&2
  fi
  git -C "$dir" cat-file -e "$commit^{commit}" || { echo "substrate.sh: $dir does not hold $commit" >&2; exit 1; }
  echo "$dir"
}

install() {
  local src kc extra=()
  step "koinon-api (host build)"
  # The binary and the admin kubeconfig of the kind cluster stay outside $OUT: CI uploads $OUT as an artifact (7 days).
  local work
  work=$(mktemp -d "${TMPDIR:-/tmp}/substrate-install.XXXXXX")
  TMP_DIRS+=("$work")
  (cd "$root" && go build -o "$work/koinon-api" ./cmd/koinon-api)
  src=$(source_clone)
  kc="$work/kubeconfig"
  kind get kubeconfig --name "$CLUSTER" >"$kc"
  chmod 600 "$kc"
  [[ -z ${GVISOR_MIRROR:-} ]] || extra+=(--gvisor-mirror "$GVISOR_MIRROR")
  step "koinon-api substrate install --platform kind (gvisor class only), context $CTX"
  # The probe pods run the koinon-api image from the local registry; the node pulls it (hosts.toml, registry.sh attach).
  KUBECONFIG=/dev/null "$work/koinon-api" substrate install \
    --substrate-source "$src" \
    --kubeconfig "$kc" --context "$CTX" \
    --probe-image "$REPO/api:e2e" \
    --storage-bucket "$BUCKET" \
    --platform kind \
    --sandbox-classes gvisor \
    ${extra[@]+"${extra[@]}"}
}

# ca: the Secret <NAMESPACE>/ate-ca with ca.crt, the roots that sign the serving certificate of ateapi. The reconciler and
# agentd verify that certificate (execution.substrate.caSecret in kind/substrate-values.yaml). The pod certificate controller
# of Substrate publishes the roots of its service-DNS signer as ClusterTrustBundles. The namespace must exist.
ca() {
  local ns=${NAMESPACE:-koinon} pem
  pem=$(kubectl --context "$CTX" get clustertrustbundles -o json |
    jq -r '[.items[] | select(.spec.signerName == "servicedns.podcert.ate.dev/identity") | .spec.trustBundle] | join("\n")')
  [[ $pem == *"BEGIN CERTIFICATE"* ]] || { echo "substrate.sh: no ClusterTrustBundle of the signer servicedns.podcert.ate.dev/identity holds a certificate" >&2; exit 1; }
  step "CA of the ateapi certificate: Secret $ns/ate-ca"
  kubectl --context "$CTX" -n "$ns" create secret generic ate-ca --from-literal=ca.crt="$pem" --dry-run=client -o yaml |
    kubectl --context "$CTX" apply -f - >&2
}

case $mode in
harness) harness ;;
install) install ;;
ca) ca ;;
esac
