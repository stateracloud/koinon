# SPDX-License-Identifier: Apache-2.0
# Spike harness: pi 1.3.0 with the pi-acp 0.0.34 adapter. Both run under node.
# dockerfile-lint: allow=digest spike under hack/spike, built by hand for an experiment and never shipped; the base is a moving node tag on purpose
FROM docker.io/library/node:24-bookworm-slim
RUN apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 -o APT::Update::Error-Mode=any update && DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 install -y --no-install-recommends ca-certificates git curl \
 && rm -rf /var/lib/apt/lists/* \
 && npm i -g --ignore-scripts @earendil-works/pi-coding-agent@1.3.0 pi-acp@0.0.34 && npm cache clean --force
COPY models.json settings.json mcp.json /etc/koinon/pi/
COPY koinon-pi /usr/local/bin/koinon-pi
RUN chmod 0755 /usr/local/bin/koinon-pi && chmod -R a+rX /etc/koinon
ENV PI_SKIP_VERSION_CHECK=1 PI_TELEMETRY=0
