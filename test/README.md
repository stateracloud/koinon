# test

End-to-end tests (kind), flavor contract tests and the sandbox escape suite (ADR 0025).

- `flavors/`: flavor contract suite. Runs each flavor image in Docker as OpenShell would and drives
  it over ACP against the fake model server. Skips without Docker or without the image
  (`make test-flavors` builds it).
- `escape/`: sandbox escape suite (ADR 0025, plan item E2), in two parts:
  - `make test-admission` (label `admission`): the chart's ValidatingAdmissionPolicies, rendered
    with `helm template` and applied to a real API server from envtest. Pod creation by anyone but
    the Agent Sandbox controller, `io.katacontainers.*` annotations, runtime classes outside the
    list, host namespaces, hostPath, privileged and ephemeral containers, spec changes, and writes
    to Secrets and Sandbox objects are denied. Runs in CI (`escape-admission` job); skipped
    without `KUBEBUILDER_ASSETS`.
  - `make test-escape` (label `openshell`): sandboxes on a real OpenShell gateway, home lab only.
    An agent sandbox as the reconciler renders it (agentgateway key and GitHub read grant
    attached) has no direct egress, no DNS but the sandbox's own, no cloud metadata, no
    service-account token or API server access, and no credential value on disk or in any
    process environment, only placeholders. The GitHub read grant cases check that push, REST
    writes and repositories outside the binding are refused by policy. Skipped without
    `KOINON_OPENSHELL_ENDPOINT`; see the package comment for the variables.

  Running the OpenShell cases on the home lab, with the gateway port-forwarded:

  ```sh
  export KOINON_OPENSHELL_ENDPOINT=127.0.0.1:18080
  export KOINON_OPENSHELL_MTLS_DIR=$XDG_CONFIG_HOME/openshell/gateways/<gateway>/mtls
  export KOINON_OPENSHELL_IMAGE=<registry>/koinon/spike-probe:1   # sh, git, curl, python3
  export KOINON_OPENSHELL_WORKSPACE=<a workspace you may create sandboxes in>
  make test-escape
  ```

  Every spec deletes the sandboxes, providers and profiles it creates. Kata isolation itself
  (the VM boundary, guest seccomp) is only exercised here, not in CI.
