# SPDX-License-Identifier: Apache-2.0
#
# The toolchain the kind e2e (test/e2e/run.sh) runs in, for hack/e2e-remote.sh: the same Go (from
# go.mod), kind, ko and helm as .github/workflows/e2e-kind.yml, plus kubectl matching the kind node
# (Kubernetes 1.37, test/e2e/kind/up.sh), jq, git, make and a Docker CLI with buildx. It has no
# daemon: the script mounts the host's Docker socket, so kind nodes and ko/buildx builds run on the
# host's Docker. Keep the versions here in step with the workflow (hack/e2e-remote-test.sh checks).
#
# GO_VERSION must equal go.mod's `go` line (hack/e2e-remote.sh refuses to build otherwise) and
# GO_DIGEST is that tag's multi-arch index digest: bump both together.
ARG GO_VERSION=1.27.3
ARG GO_DIGEST=sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9

FROM docker:29-cli@sha256:5e6baf67325a79f64ed3bed6863954d5e05d0f3d4e57cbdb14809346814bbfdb AS dockercli
FROM docker/buildx-bin:0.30.1@sha256:cc16bd35c1bbdde2c9d4ee08cf33be55b17e6ed2188816292dbde51064105e25 AS buildx

FROM golang:${GO_VERSION}-bookworm@${GO_DIGEST}
ARG TARGETARCH
ARG KIND_VERSION=v0.33.0
ARG KO_VERSION=0.19.1
ARG HELM_VERSION=v4.3.1
ARG KUBECTL_VERSION=v1.37.0
# SHA-256 of the release artifacts, per architecture (amd64 values for kind and ko are the ones the
# workflow pins).
ARG KIND_SHA256_amd64=aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d
ARG KIND_SHA256_arm64=20022bee6cfcd5086cb7234d218e3454e6090022f2a8f55d1fa7fcf42c3867a2
ARG KO_SHA256_amd64=635ac6ea3fd376c935fee597fbb29ab2c2449f49ef1655085fe3aa9c25fed7a5
ARG KO_SHA256_arm64=4099b2d1170d3b8a70e049237462efc2dd14d5fa30e9d2e5e108fb4f778cdd3f
ARG HELM_SHA256_amd64=0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b
ARG HELM_SHA256_arm64=8cf8f66ecbc252ac8f7f06cca68aba88508ddfde3a6a34feb1678ad1694b4f88
ARG KUBECTL_SHA256_amd64=6129359f4e1f3848a5572ccb0b26cf28b8ca08cef38c95a765b2f64a2c961a2f
ARG KUBECTL_SHA256_arm64=922df28df248cc00a9e025f947704f1d1482de64ece54cfe57e61f19eaf1eef3

# GOTOOLCHAIN=local: the image's Go is the one go.mod asks for; never download another.
ENV GOTOOLCHAIN=local

# The apt packages are not version-pinned (hadolint DL3008) on purpose: this is a local dev and lab
# toolchain, not a shipped image, and the base is pinned by digest. Debian bookworm's archive drops
# superseded versions, so an exact pin would break the build as soon as a security update lands,
# while the flavor images pin against an Ubuntu snapshot (flavors/opencode/Dockerfile) because they
# ship. Everything that decides the e2e's behaviour (go, kind, ko, helm, kubectl, buildx) is pinned
# above by version and sha256.
# hadolint ignore=DL3008
RUN apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 -o APT::Update::Error-Mode=any update \
 && apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 install -y --no-install-recommends ca-certificates curl git jq make netcat-openbsd openssl procps unzip \
 && rm -rf /var/lib/apt/lists/*

COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=buildx /buildx /usr/local/lib/docker/cli-plugins/docker-buildx

# The optional BuildKit secret github_token (--secret id=github_token,env=GITHUB_TOKEN) authenticates
# the github.com release downloads (#145); without it they are anonymous. The github.com web URL
# ignores a token, so an authenticated download goes through the REST API (release by tag, asset by
# id, Accept: application/octet-stream), as in flavors/common/koinon-fetch (this build context cannot
# copy that script). curl reads the header from stdin, so the token is in no argv, env var, log or layer.
RUN --mount=type=secret,id=github_token,required=false set -eu; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    sha() { eval "echo \$$1_$arch"; }; \
    token=; [ ! -r /run/secrets/github_token ] || token=$(tr -d '\r\n' </run/secrets/github_token); \
    authed() { printf 'header = "Authorization: Bearer %s"\n' "$token" | curl -fsSL --retry 3 --retry-all-errors -K - "$@"; }; \
    fetch() { \
      if [ -n "$token" ] && [ "${2#https://github.com/}" != "$2" ]; then \
        r=${2#https://github.com/}; repo=${r%%/releases/download/*}; r=${r#*/releases/download/}; tag=${r%%/*}; name=${r#*/}; \
        id=$(authed -H 'Accept: application/vnd.github+json' "https://api.github.com/repos/$repo/releases/tags/$tag" | jq -er --arg n "$name" '.assets[] | select(.name == $n) | .id'); \
        authed -H 'Accept: application/octet-stream' -D /tmp/fetch.headers -o "$1" "https://api.github.com/repos/$repo/releases/assets/$id"; \
        grep -i '^x-ratelimit-limit' /tmp/fetch.headers | sed 's/^/fetch: authenticated, /' || true; rm -f /tmp/fetch.headers; \
      else \
        curl -fsSL --retry 3 --retry-all-errors -o "$1" "$2"; \
      fi; \
      echo "$3  $1" | sha256sum -c -; \
    }; \
    fetch /usr/local/bin/kind "https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-linux-${arch}" "$(sha KIND_SHA256)"; \
    fetch /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${arch}/kubectl" "$(sha KUBECTL_SHA256)"; \
    chmod +x /usr/local/bin/kind /usr/local/bin/kubectl; \
    ko_arch=x86_64; [ "$arch" = arm64 ] && ko_arch=arm64; \
    fetch /tmp/ko.tgz "https://github.com/ko-build/ko/releases/download/v${KO_VERSION}/ko_${KO_VERSION}_Linux_${ko_arch}.tar.gz" "$(sha KO_SHA256)"; \
    tar xzf /tmp/ko.tgz -C /usr/local/bin ko; \
    fetch /tmp/helm.tgz "https://get.helm.sh/helm-${HELM_VERSION}-linux-${arch}.tar.gz" "$(sha HELM_SHA256)"; \
    tar xzf /tmp/helm.tgz -C /usr/local/bin --strip-components=1 "linux-${arch}/helm"; \
    rm -f /tmp/ko.tgz /tmp/helm.tgz

# The mounted checkout belongs to another user; git must still read it.
RUN git config --system --add safe.directory '*'

WORKDIR /work
