# SPDX-License-Identifier: Apache-2.0
# Spike probe image: the OpenShell default base plus the tools the credential,
# policy and escape checks need. Not product code.
# BASE has no default here: test/escape/kind.sh always passes it, from KOINON_PROBE_BASE or else the
# digest pin of flavors/opencode/Dockerfile (ARG BASE=), the one place the base is defined. The e2e
# (test/e2e/escape-openshell.sh) pulls the published probe image (#482, tag from hack/probe-tag.sh) and builds
# this file only on a miss, on that same pinned base; the flavor's USER and ENTRYPOINT are reset below.
ARG BASE
FROM ${BASE}
USER root
ENTRYPOINT []
RUN apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 -o APT::Update::Error-Mode=any update \
 && DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 install -y --no-install-recommends \
      ca-certificates curl git python3 gdb procps binutils iproute2 dnsutils netcat-openbsd \
 && rm -rf /var/lib/apt/lists/*
