# syntax=docker/dockerfile:1.19@sha256:b6afd42430b15f2d2a4c5a02b919e98a525b785b1aaff16747d2f623364e39b6
# SPDX-License-Identifier: Apache-2.0
#
# Substrate overlay of a Koinon harness image (ADR 0119, ADR 0015 and 0016 as amended, issue #1087):
# the harness image with koinon-guest-launcher and its policy added, and the launcher as the
# entrypoint wrapper. Only the substrate driver uses this image. The OpenShell driver (the default)
# keeps the plain harness image and its exec path, so nothing changes for it.
#
# Build: make harness-substrate-opencode. The launcher binary comes from the build context
# "launcher" (build/guest-launcher/linux-<arch>/koinon-guest-launcher, built by `make guest-launcher`);
# the harness image is FLAVOR_IMAGE. The guest server is a Substrate component that is not in this
# repository; it joins this overlay when the substrate driver takes it (#1088).
ARG FLAVOR_IMAGE
FROM ${FLAVOR_IMAGE}
ARG TARGETARCH
# "launcher" is a named build context (--build-context launcher=build/guest-launcher), not a stage
# hadolint ignore=DL3022
COPY --from=launcher --chmod=0755 linux-${TARGETARCH}/koinon-guest-launcher /usr/local/bin/koinon-guest-launcher
COPY --chmod=0644 policy.json /etc/koinon/guest-launcher/policy.json
# The payload is the harness's entry script (koinon-opencode here); another harness gets its own overlay line.
ENTRYPOINT ["/usr/local/bin/koinon-guest-launcher", "--policy", "/etc/koinon/guest-launcher/policy.json", "--", "/usr/local/bin/koinon-opencode"]
