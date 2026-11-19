# syntax=docker/dockerfile:1.19@sha256:b6afd42430b15f2d2a4c5a02b919e98a525b785b1aaff16747d2f623364e39b6
# SPDX-License-Identifier: Apache-2.0
#
# Substrate overlay of a Koinon harness image (ADR 0119, ADR 0015 and 0016 as amended, issue #1087):
# the harness image with koinon-guest-launcher and its two policies added. The contract is an exec prefix
# (harness.Descriptor.LauncherPrefix): the substrate driver runs the harness over the guest's
# ProcessService, so the ENTRYPOINT below is a default only and does not harden those execs. Only the substrate driver uses this image. The OpenShell driver (the default)
# keeps the plain harness image and its exec path, so nothing changes for it.
#
# Build: make harness-substrate-opencode. The launcher binary comes from the build context
# "launcher" (build/guest-launcher/linux-<arch>/koinon-guest-launcher, built by `make guest-launcher`);
# the harness image is FLAVOR_IMAGE. The guest server is a Substrate component that is not in this
# repository; it joins this overlay when the substrate driver takes it (#1088).
ARG FLAVOR_IMAGE
# The harness's entry script (ADR 0048), set by `make harness-substrate-<name>`.
ARG ENTRY_SCRIPT=/usr/local/bin/koinon-opencode
FROM ${FLAVOR_IMAGE}
ARG TARGETARCH
# Declared again: an ARG before FROM is not visible inside the stage.
ARG ENTRY_SCRIPT
# "launcher" is a named build context (--build-context launcher=build/guest-launcher), not a stage
# hadolint ignore=DL3022
COPY --from=launcher --chmod=0755 linux-${TARGETARCH}/koinon-guest-launcher /usr/local/bin/koinon-guest-launcher
# Two policies, one per tier family (ADR 0120 section 4.2 and 4.3): the vm tier fails closed without
# Landlock, the gVisor tiers (hardened, dev) run it best effort behind the read-only root.
COPY --chmod=0644 policy-vm.json policy-gvisor.json /etc/koinon/guest-launcher/
# An exec-form ENTRYPOINT cannot expand a build argument, so a fixed path links to the harness's entry script.
# The harness image ends as uid 10001 (harnesses/opencode/Dockerfile); /usr/local/bin belongs to root.
USER root
RUN ln -s "${ENTRY_SCRIPT}" /usr/local/bin/koinon-entry
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/koinon-guest-launcher", "--policy", "/etc/koinon/guest-launcher/policy-vm.json", "--", "/usr/local/bin/koinon-entry"]
