# syntax=docker/dockerfile:1.19@sha256:b6afd42430b15f2d2a4c5a02b919e98a525b785b1aaff16747d2f623364e39b6
# SPDX-License-Identifier: Apache-2.0
#
# Koinon OpenCode flavor (ADR 0035, 0048, 0052): OpenCode driven over ACP by koinon-agentd through
# OpenShell's non-TTY exec. No Koinon binary is in this image (ADR 0049).
#
# Build: make flavor-opencode (docker buildx, linux/amd64 and linux/arm64).

# OpenShell's default sandbox base (ADR 0035), multi-arch index digest, checked 2026-10-22.
ARG BASE=nvcr.io/nvidia/base/ubuntu:24.04@sha256:7e0f726d635d440d28a7f18e6498d3bdb76c7038bd4c90a090c21eb7c9f40686

# OpenCode 2.2.0 (ADR 0052) is the compiled glibc build that npm's @opencode/cli package installs
# as an optional dependency (@opencode/cli-linux-<arch>). The sha256 values below were computed
# from the registry tarballs after checking them against npm's published sha512 integrity:
#   x64:   sha512-XTGFTiZTgugps6rgTXp4mPjOM8klsa5uHn/Shcms2o3k1bjSA3Sgl6O4yJ48y18WWxn9My5Pxt7JKcLID7LOHg==
#   arm64: sha512-NCCltb8h/8WL30X9o2KTm3UFX0lmfk48lG8tglXJN9moscxeB7w2qOmfHVa32iFngk0iVAKH4xRPlBc88Y0qlw==
# Re-pinning is issue #54: the entry script and README name 2.2.0-specific workarounds.
FROM ${BASE} AS fetch-amd64
ADD --checksum=sha256:fe3046b86872e624f62268058f4801a1f1efffc193103a9988238c891b8ca028 \
    https://registry.npmjs.org/@opencode/cli-linux-x64/-/cli-linux-x64-2.2.0.tgz /tmp/opencode.tgz

FROM ${BASE} AS fetch-arm64
ADD --checksum=sha256:6a40643c0dca3aff9d37c87c26c0ee003c46abb810e1c6ddde094ffbf7460b8b \
    https://registry.npmjs.org/@opencode/cli-linux-arm64/-/cli-linux-arm64-2.2.0.tgz /tmp/opencode.tgz

FROM fetch-${TARGETARCH} AS fetch
RUN tar -xzf /tmp/opencode.tgz -C /tmp package/bin/opencode \
 && install -m 0755 /tmp/package/bin/opencode /opencode \
 && /opencode --version | grep -qx 'opencode v2.2.0'

FROM ${BASE}
# git for the repository the agent works on, ripgrep so OpenCode does not download its own
# (it uses rg from PATH when present), ca-certificates for TLS through the OpenShell proxy.
# Package versions follow the Ubuntu archive at build time; the base is pinned by digest.
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates git ripgrep \
 && rm -rf /var/lib/apt/lists/*
# OpenShell runs the sandbox as UID/GID 10001 with the home volume at /sandbox (spike 1, 8).
RUN groupadd --gid 10001 sandbox \
 && useradd --uid 10001 --gid 10001 --home-dir /sandbox --no-create-home --shell /bin/bash sandbox \
 && install -d -o 10001 -g 10001 -m 0750 /sandbox
COPY --from=fetch /opencode /usr/local/bin/opencode
# The entry scripts' shared helpers (flavors/common, build context "common").
COPY --from=common --chmod=0755 koinon-entry-lib.sh /usr/local/lib/koinon/koinon-entry-lib.sh
COPY --chmod=0755 koinon-opencode /usr/local/bin/koinon-opencode
COPY --chmod=0644 etc/koinon/opencode/opencode.jsonc /etc/koinon/opencode/opencode.jsonc
# git's GitHub credential helper, shared by every flavor (flavors/common, build context
# "common"): it prints the OpenShell placeholder and stores nothing.
COPY --from=common --chmod=0755 koinon-git-credential /usr/local/bin/koinon-git-credential
COPY --from=common --chmod=0644 etc/koinon/git/config /etc/koinon/git/config
RUN printf '[include]\n\tpath = /etc/koinon/git/config\n\tpath = /sandbox/.koinon/git/config\n' >> /etc/gitconfig
# Koinon's instructions are per run: koinon-agentd writes /sandbox/.koinon/AGENTS.md and the
# config's "instructions" key names it (see README.md).
RUN chmod -R a+rX,go-w /etc/koinon /usr/local/lib/koinon
USER 10001:10001
WORKDIR /sandbox
ENTRYPOINT ["/usr/local/bin/koinon-opencode"]
