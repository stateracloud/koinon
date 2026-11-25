# syntax=docker/dockerfile:1.19@sha256:b6afd42430b15f2d2a4c5a02b919e98a525b785b1aaff16747d2f623364e39b6
# SPDX-License-Identifier: Apache-2.0
#
# Substrate overlay of a Koinon harness image (ADR 0119, ADR 0015 and 0016 as amended, issue #1087):
# the harness image with the Substrate guest server (ate-env-guest) and koinon-guest-launcher with its two
# policies added. The actor's process is the guest server: Substrate starts the image ENTRYPOINT, and the
# ProcessService of that server runs the harness. The launcher contract is an exec prefix
# (harness.Descriptor.LauncherPrefix): the substrate driver runs the harness over the ProcessService, so the
# harness is a child of the guest server and never of the ENTRYPOINT. Only the substrate driver uses this
# image. The OpenShell driver (the default) keeps the plain harness image and its exec path.
#
# Build: make harness-substrate-opencode. Both binaries come from named build contexts: "launcher"
# (build/guest-launcher/linux-<arch>/koinon-guest-launcher, `make guest-launcher`) and "guest"
# (build/env-guest/linux-<arch>/ate-env-guest, `make env-guest`); the harness image is FLAVOR_IMAGE. The
# release builds the same image with hack/release-harness-substrate.sh. There is no RUN step, so a
# multi-platform build needs no emulation.
ARG FLAVOR_IMAGE
FROM ${FLAVOR_IMAGE}
ARG TARGETARCH
# "launcher" and "guest" are named build contexts, not stages
# hadolint ignore=DL3022
COPY --from=launcher --chmod=0755 linux-${TARGETARCH}/koinon-guest-launcher /usr/local/bin/koinon-guest-launcher
# hadolint ignore=DL3022
COPY --from=guest --chmod=0755 linux-${TARGETARCH}/ate-env-guest /usr/local/bin/ate-env-guest
# Two policies, one per tier family (ADR 0120 section 4.2 and 4.3): the vm tier fails closed without
# Landlock, the gVisor tiers (hardened, dev) run it best effort (their root is writable by decision, issue #1150).
COPY --chmod=0644 policy-vm.json policy-gvisor.json /etc/koinon/guest-launcher/
# The guest server listens on port 80 (its default, which the Substrate router expects) and runs the payload as
# its own user, so it runs as root: a non-root user has neither the port nor the right to start a process as
# another user. The payload is hardened by the launcher (no_new_privs, capability drop, seccomp, Landlock), and
# the user mode stays best_effort until Substrate has a payload uid/gid option (ADR 0120 section 4, decision 2026-11-18).
USER root
WORKDIR /sandbox
ENTRYPOINT ["/usr/local/bin/ate-env-guest", "-workspace", "/sandbox"]
