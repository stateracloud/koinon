# SPDX-License-Identifier: Apache-2.0
# Spike probe image: the OpenShell default base plus the tools the credential,
# policy and escape checks need. Not product code.
# BASE is the OpenShell default sandbox base, pinned like the flavors' (flavors/opencode/Dockerfile). The
# e2e (test/e2e/escape-openshell.sh) passes the OpenCode flavor image it has already built or pulled, so the
# step needs no registry the e2e does not use otherwise; the flavor's USER and ENTRYPOINT are reset below.
ARG BASE=nvcr.io/nvidia/base/ubuntu:24.04@sha256:7e0f726d635d440d28a7f18e6498d3bdb76c7038bd4c90a090c21eb7c9f40686
FROM ${BASE}
USER root
ENTRYPOINT []
RUN apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 -o APT::Update::Error-Mode=any update \
 && DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 install -y --no-install-recommends \
      ca-certificates curl git python3 gdb procps binutils iproute2 dnsutils netcat-openbsd \
 && rm -rf /var/lib/apt/lists/*
