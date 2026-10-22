# syntax=docker/dockerfile:1.19@sha256:b6afd42430b15f2d2a4c5a02b919e98a525b785b1aaff16747d2f623364e39b6
# SPDX-License-Identifier: Apache-2.0
#
# Koinon Hermes flavor (ADR 0035, 0048): Hermes Agent driven over ACP by koinon-agentd through
# OpenShell's non-TTY exec. No Koinon binary is in this image (ADR 0049).
#
# Build: make flavor-hermes (docker buildx, linux/amd64 and linux/arm64).

# The official Hermes Agent image, v0.22.3 (commit 5b147ab036d6d19131bff8d20701b5001eab844a,
# from its install-stamp.json), multi-arch index digest, checked 2026-10-22. It carries Hermes
# with its Python 3.14 virtual environment in /opt/hermes, git and ripgrep, on Debian 13. Its
# s6 supervisor and entrypoint are not used: OpenShell runs the sandbox's own process, and
# koinon-agentd execs koinon-hermes.
FROM docker.io/nousresearch/hermes-agent:v0.22.3@sha256:2f305c9bdc81731f48664586d23cb6a6f838a15ad7be1bfabe0c3c269d8e09b9
USER root
# OpenShell runs the sandbox as UID/GID 10001 with the home volume at /sandbox (spike 1, 8);
# the image's own hermes user (10000) is not used.
RUN groupadd --gid 10001 sandbox \
 && useradd --uid 10001 --gid 10001 --home-dir /sandbox --no-create-home --shell /bin/bash sandbox \
 && install -d -o 10001 -g 10001 -m 0750 /sandbox \
 && /opt/hermes/.venv/bin/hermes --version | grep -q 'v0\.22\.3'
COPY --chmod=0755 koinon-hermes /usr/local/bin/koinon-hermes
COPY --chmod=0644 etc/koinon/hermes/config.yaml /etc/koinon/hermes/config.yaml
# /opt/hermes is already world-readable; the read-only root file system keeps it unchanged.
RUN chmod -R a+rX,go-w /etc/koinon
USER 10001:10001
WORKDIR /sandbox
ENTRYPOINT ["/usr/local/bin/koinon-hermes"]
