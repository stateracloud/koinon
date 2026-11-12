# SPDX-License-Identifier: Apache-2.0
# Spike harness: Hermes Agent v0.22.3 from its official image (Python 3.14 in /opt/hermes).
# HERMES_HOME moves state to the sandbox workspace; the config is copied there on start.
FROM docker.io/nousresearch/hermes-agent:v0.22.3
USER root
COPY config.yaml /etc/koinon/hermes/config.yaml
COPY koinon-hermes /usr/local/bin/koinon-hermes
RUN chmod 0755 /usr/local/bin/koinon-hermes && chmod -R a+rX /etc/koinon /opt/hermes
ENV HERMES_HOME=/sandbox/.hermes
ENTRYPOINT []
