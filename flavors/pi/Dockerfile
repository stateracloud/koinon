# syntax=docker/dockerfile:1.19@sha256:b6afd42430b15f2d2a4c5a02b919e98a525b785b1aaff16747d2f623364e39b6
# SPDX-License-Identifier: Apache-2.0
#
# Koinon pi flavor (ADR 0035, 0048): pi driven over ACP through the pi-acp adapter by
# koinon-agentd through OpenShell's non-TTY exec. No Koinon binary is in this image (ADR 0049).
#
# Build: make flavor-pi (docker buildx, linux/amd64 and linux/arm64).

# OpenShell's default sandbox base (ADR 0035), multi-arch index digest, checked 2026-10-22.
ARG BASE=nvcr.io/nvidia/base/ubuntu:24.04@sha256:7e0f726d635d440d28a7f18e6498d3bdb76c7038bd4c90a090c21eb7c9f40686

# Node.js 24.22.1 (LTS), the official linux tarballs; sha256 from
# https://nodejs.org/dist/v24.22.1/SHASUMS256.txt.
FROM ${BASE} AS node-amd64
ADD --checksum=sha256:6c172a9ab6718f7b801fb5a7bd486af2ee5e32758086a04b84fcce2aa77095e6 \
    https://nodejs.org/dist/v24.22.1/node-v24.22.1-linux-x64.tar.xz /tmp/node.tar.xz

FROM ${BASE} AS node-arm64
ADD --checksum=sha256:dbabf7a1cbf947e6059ffad2c27b80041db97dea1e553b62afdee7f13fca336e \
    https://nodejs.org/dist/v24.22.1/node-v24.22.1-linux-arm64.tar.xz /tmp/node.tar.xz

FROM node-${TARGETARCH} AS node
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends xz-utils \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir /node \
 && tar -xJf /tmp/node.tar.xz -C /node --strip-components=1 \
 && /node/bin/node --version | grep -qx 'v24.22.1'

# pi 1.3.0 and pi-acp 0.0.34 with every dependency pinned by version and sha512 integrity in
# npm/package-lock.json; npm ci refuses a tarball whose integrity does not match. Install
# scripts are off: none of the packages needs one on Linux.
FROM node AS pi
COPY npm/package.json npm/package-lock.json /opt/pi/
RUN cd /opt/pi \
 && PATH=/node/bin:$PATH npm ci --omit=dev --ignore-scripts --no-audit --no-fund \
 && PATH=/node/bin:$PATH npm cache clean --force \
 && /node/bin/node /opt/pi/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js --version | grep -qx '1.3.0'

FROM ${BASE}
# git for the repository the agent works on, ripgrep and fd for pi's search tools (it uses
# them from PATH instead of downloading its own), ca-certificates for TLS through the
# OpenShell proxy. Package versions follow the Ubuntu archive at build time; the base is pinned
# by digest.
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates git ripgrep fd-find \
 && rm -rf /var/lib/apt/lists/* \
 && ln -s /usr/bin/fdfind /usr/local/bin/fd
# OpenShell runs the sandbox as UID/GID 10001 with the home volume at /sandbox (spike 1, 8).
RUN groupadd --gid 10001 sandbox \
 && useradd --uid 10001 --gid 10001 --home-dir /sandbox --no-create-home --shell /bin/bash sandbox \
 && install -d -o 10001 -g 10001 -m 0750 /sandbox
# Node without npm, npx and corepack: pi-acp runs `npm view` on every session/new to look for
# a newer pi (no switch), and nothing in the sandbox should install packages.
COPY --from=node /node/bin/node /opt/node/bin/node
COPY --from=pi /opt/pi /opt/pi
COPY --chmod=0755 koinon-pi /usr/local/bin/koinon-pi
COPY --chmod=0755 koinon-pi-rpc /usr/local/lib/koinon/koinon-pi-rpc
COPY --chmod=0644 koinon-pi-render.mjs /usr/local/lib/koinon/koinon-pi-render.mjs
COPY --chmod=0644 etc/koinon/pi/ /etc/koinon/pi/
RUN chmod -R a+rX,go-w /etc/koinon /opt/node /opt/pi /usr/local/lib/koinon
USER 10001:10001
WORKDIR /sandbox
ENTRYPOINT ["/usr/local/bin/koinon-pi"]
