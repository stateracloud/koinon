# syntax=docker/dockerfile:1.19@sha256:b6afd42430b15f2d2a4c5a02b919e98a525b785b1aaff16747d2f623364e39b6
# SPDX-License-Identifier: Apache-2.0
#
# Koinon OpenCode flavor (ADR 0035, 0048): OpenCode driven over ACP by koinon-agentd through
# OpenShell's non-TTY exec. No Koinon binary is in this image (ADR 0049).
#
# Build: make flavor-opencode (docker buildx, linux/amd64 and linux/arm64).

# OpenShell's default sandbox base (ADR 0035), multi-arch index digest, checked 2026-10-22.
ARG BASE=nvcr.io/nvidia/base/ubuntu:24.04@sha256:7e0f726d635d440d28a7f18e6498d3bdb76c7038bd4c90a090c21eb7c9f40686

# OpenCode 1.19.3 is the compiled glibc build that npm's opencode-ai package installs as an
# optional dependency (opencode-linux-<arch>). The sha256 values below were computed from the
# registry tarballs after checking them against npm's published sha512 integrity:
#   x64:   sha512-wz+kXi7ZkvLW04hEIocW3RoEznALJ3YGnvNlZdXFarYF0v1jyDyAwpPGZkxb4JZDrw4oy48/LDZ8wCFIQDTxhw==
#   arm64: sha512-Y1HIsJNK+zxbtd54TCrEI/G3m/RSALCaKbzAaaxJYF+ZHQE2PFlHmueqWDigQoT1tbiPi/8tPMirxpGgcXEprg==
FROM ${BASE} AS fetch-amd64
ADD --checksum=sha256:71d67513eb4078a7ff8eb96806b1d5edcd6533a72b311409b2e88bad4188dac6 \
    https://registry.npmjs.org/opencode-linux-x64/-/opencode-linux-x64-1.19.3.tgz /tmp/opencode.tgz

FROM ${BASE} AS fetch-arm64
ADD --checksum=sha256:2c97235c616d859b01c742744c6182d4c90512b34a914d1a17bdf47bd2fa70bc \
    https://registry.npmjs.org/opencode-linux-arm64/-/opencode-linux-arm64-1.19.3.tgz /tmp/opencode.tgz

FROM fetch-${TARGETARCH} AS fetch
RUN tar -xzf /tmp/opencode.tgz -C /tmp package/bin/opencode \
 && install -m 0755 /tmp/package/bin/opencode /opencode \
 && /opencode --version | grep -qx '1.19.3'

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
# OpenCode writes a .gitignore into each config directory that lacks one; ship it so the
# read-only directory needs no write.
RUN printf '%s\n' node_modules package.json package-lock.json bun.lock .gitignore \
      > /etc/koinon/opencode/.gitignore \
 && chmod -R a+rX,go-w /etc/koinon
USER 10001:10001
WORKDIR /sandbox
ENTRYPOINT ["/usr/local/bin/koinon-opencode"]
