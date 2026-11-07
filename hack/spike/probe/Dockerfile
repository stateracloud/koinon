# SPDX-License-Identifier: Apache-2.0
# Spike probe image: the OpenShell default base plus the tools the credential,
# policy and escape checks need. Not product code.
FROM nvcr.io/nvidia/base/ubuntu:24.04
RUN apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 -o APT::Update::Error-Mode=any update \
 && DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 install -y --no-install-recommends \
      ca-certificates curl git python3 gdb procps binutils iproute2 dnsutils netcat-openbsd \
 && rm -rf /var/lib/apt/lists/*
