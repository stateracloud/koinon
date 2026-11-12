# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the sandbox pod-field rules and the Agent Sandbox extension kinds (issue 673).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

sp=templates/sandbox-policy.yaml

# pods/resize changes container resources outside a pod UPDATE: the pods rule covers it, so only
# the Agent Sandbox controller may resize a sandbox pod.
has "the sandbox pods rule covers pods/resize" 'resources: \[pods, pods/ephemeralcontainers, pods/resize\]' --show-only "$sp"

# Pod fields the Pod Security baseline forbids and OpenShell 0.1.7 never sets.
has "sandbox pods may not add Linux capabilities" 'sandbox pods must not add Linux capabilities' --show-only "$sp"
has "sandbox pods keep the RuntimeDefault seccomp profile" 'sandbox pods must use the RuntimeDefault seccomp profile' --show-only "$sp"
has "sandbox pods may not set a hostPort" 'hostPort is not allowed in sandbox pods' --show-only "$sp"
has "the pod-field rules cover init containers" 'name: allContainers' --show-only "$sp"

# The Agent Sandbox extension kinds: only the controller writes them in sandbox namespaces.
has "the objects policy covers the extension API group" 'apiGroups: \[extensions.agents.x-k8s.io\]' --show-only "$sp"
has "the objects policy covers every extension kind" 'resources: \[sandboxtemplates, sandboxclaims, sandboxwarmpools\]' --show-only "$sp"
has "the extension kinds take the controller identity" 'variables.isExtension \? \[\\"system:serviceaccount:agent-sandbox-system:agent-sandbox-controller\\"\]' --show-only "$sp"
has "a changed controller identity reaches the extension kinds" 'variables.isExtension \? \[\\"system:serviceaccount:as:ctl\\"\]' \
	--set 'sandboxPolicy.identities.pods={system:serviceaccount:as:ctl}' --show-only "$sp"
has "the extension kinds fence other identities" 'only the Agent Sandbox controller may write ' --show-only "$sp"
