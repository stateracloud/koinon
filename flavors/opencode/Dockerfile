# syntax=docker/dockerfile:1.19@sha256:b6afd42430b15f2d2a4c5a02b919e98a525b785b1aaff16747d2f623364e39b6
# SPDX-License-Identifier: Apache-2.0
#
# Koinon OpenCode flavor (ADR 0035, 0048, 0052): OpenCode driven over ACP by koinon-agentd through
# OpenShell's non-TTY exec. No Koinon binary is in this image (ADR 0049).
#
# Build: make flavor-opencode (docker buildx, linux/amd64 and linux/arm64).

# OpenShell's default sandbox base (ADR 0035), multi-arch index digest, checked 2026-10-22.
ARG BASE=nvcr.io/nvidia/base/ubuntu:24.04@sha256:7e0f726d635d440d28a7f18e6498d3bdb76c7038bd4c90a090c21eb7c9f40686

# OpenCode 2.0.24 (ADR 0052) is the compiled glibc build that npm's @opencode/cli package installs
# as an optional dependency (@opencode/cli-linux-<arch>). The sha256 values below were computed
# from the registry tarballs after checking them against npm's published sha512 integrity:
#   x64:   sha512-WDBunGoImJOYjwUvDDfl0GJiKR/WbATnSXW1QTrK+VDp+oOUNLXo84QTUCBVUqvb8VsseWhs3uyWCD9o9c5JDA==
#   arm64: sha512-n+C1XkoIB0NaHK9CQo4gGIc9FRGDhrMdCXO8heEOvUgKsH/0ROxCr3CxUuJaKolL/5+c5u3CoiXsckL+fa52ag==
# Re-pinning is issue #85: the entry script and README name 2.0.24-specific workarounds.
FROM ${BASE} AS fetch-amd64
ADD --checksum=sha256:21b1ee0683841405d69541fc44481f8e5752e95bea72e8b8ec31dbcdb102e7f8 \
    https://registry.npmjs.org/@opencode/cli-linux-x64/-/cli-linux-x64-2.0.24.tgz /tmp/opencode.tgz

FROM ${BASE} AS fetch-arm64
ADD --checksum=sha256:9d0cd2bfc060fd6c2afdf6db69837f8690d46a8d8327bb864421851ce004f453 \
    https://registry.npmjs.org/@opencode/cli-linux-arm64/-/cli-linux-arm64-2.0.24.tgz /tmp/opencode.tgz

FROM fetch-${TARGETARCH} AS fetch
RUN tar -xzf /tmp/opencode.tgz -C /tmp package/bin/opencode \
 && install -m 0755 /tmp/package/bin/opencode /opencode \
 && /opencode --version | grep -qx 'opencode v2.0.24'

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
COPY --chmod=0755 koinon-opencode /usr/local/bin/koinon-opencode
COPY --chmod=0644 etc/koinon/opencode/opencode.jsonc /etc/koinon/opencode/opencode.jsonc
# OpenCode loads AGENTS.md from its global config directory into the system prompt. Koinon's
# instructions are per run, so koinon-agentd writes them into the home and this link points there
# (2.0.24 ignores the "instructions" config key, see README.md).
RUN ln -s /sandbox/.koinon/AGENTS.md /etc/koinon/opencode/AGENTS.md \
 && chmod -R a+rX,go-w /etc/koinon
USER 10001:10001
WORKDIR /sandbox
ENTRYPOINT ["/usr/local/bin/koinon-opencode"]
