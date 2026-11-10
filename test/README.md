# test

End-to-end tests (kind), flavor contract tests and the sandbox escape suite (ADR 0025).

- `e2e/`: the quickstart on kind (plan item E1). `make test-e2e-kind` (or `test/e2e/run.sh`)
  creates the kind cluster `koinon-e1` with the prerequisites (`e2e/kind/up.sh`: kube-network-policies,
  CNPG, Agent Sandbox, RuntimeClass `runc`, OpenShell), builds the images with ko and buildx into
  it, deploys the fake model server and the fake GitHub (`cmd/koinon-fakegithub`), runs
  `helm install`, and drives login, connection, agent, ticket, approval and pull request through
  the API, printing the wall clock of each step. It deletes the cluster unless `KEEP=1`. Without
  `KOINON_E2E_API_URL` the Go suite skips. CI: `.github/workflows/e2e-kind.yml` (manual).

- `flavors/`: flavor contract suite. Runs each flavor image in Docker as OpenShell would and drives
  it over ACP against the fake model server. It needs the flavor images, so `make test` (and CI's
  unit job) leave it out; `make test-flavors` builds the images and then runs it. Needs Docker.
- `escape/`: sandbox escape suite (ADR 0025, plan item E2), in two parts:
  - `make test-admission` (label `admission`): the chart's ValidatingAdmissionPolicies, rendered
    with `helm template` and applied to a real API server from envtest. Pod creation by anyone but
    the Agent Sandbox controller, `io.katacontainers.*` annotations, runtime classes outside the
    list, ServiceAccount tokens on workload pods (automount not explicitly false, or a projected
    serviceAccountToken source), host namespaces, hostPath, privileged and ephemeral containers,
    spec changes, and writes
    to Secrets and Sandbox objects are denied. Runs in CI (`escape-admission` job); skipped
    without `KUBEBUILDER_ASSETS`.
  - `make test-escape-kind` (label `openshell`): the same cases against the OpenShell gateway that
    `make dev-kind-up` installs on kind-koinon-dev. It builds the probe image from `hack/spike/probe` (or pulls `$PROBE_PULL_REPO:<hack/probe-tag.sh>`, which the e2e does),
    loads it into the node, port-forwards the gateway and reads the client certificate from its
    Secret, so it needs no registry and no home lab (Docker, kind, kubectl, jq). kind runs
    sandboxes under runc: the Kata guest case and the cases that need extra fixtures (the model
    gateway stand-in, the live admission case) skip with their reasons. About 2.5 minutes; 20 cases
    pass, 4 skip. The kind e2e runs the same cases (except the Gitea ones) against the chart
    install, where the chart's sandbox admission policy is live and the live admission case runs
    and must not skip (`test/e2e/escape-openshell.sh`, #109); about 4 to 8 minutes more.
  - `make test-escape` (label `openshell`): sandboxes on any real OpenShell gateway you point it at.
    An agent sandbox as the reconciler renders it (agentgateway key and GitHub read grant
    attached) has no direct egress, no DNS but the sandbox's own, no cloud metadata, no
    service-account token or API server access, and no credential value on disk or in any
    process environment, only placeholders. The GitHub read grant cases check that push, REST
    writes and repositories outside the binding are refused by policy. Skipped without
    `KOINON_OPENSHELL_ENDPOINT`; see the package comment for the variables.

  - Leak guard exfiltration (label `leakguard`, `exfil_test.go`): from a sandbox, a `write_item`
    call carrying what `read_item` returned, and a `fetch_url` to an attacker address, must be refused or
    parked and never reach the fake receiver. Needs a guarded gateway the suite does not start
    (`KOINON_ESCAPE_MCP_HOST`, `KOINON_ESCAPE_MCP_KEY`, `KOINON_ESCAPE_MCP_RECEIVER_URL`); skipped
    without them. CI runs it inside the kind e2e (`ESCAPE_MCP=1`, `test/e2e/escape-mcp.sh`, #348).
    `docs/dev/ci.md` has the details.

  Running the OpenShell cases against another cluster's gateway, port-forwarded:

  ```sh
  export KOINON_OPENSHELL_ENDPOINT=127.0.0.1:18080
  export KOINON_OPENSHELL_MTLS_DIR=$XDG_CONFIG_HOME/openshell/gateways/<gateway>/mtls
  export KOINON_OPENSHELL_IMAGE=<registry>/koinon/spike-probe:1   # sh, git, curl, python3
  export KOINON_OPENSHELL_WORKSPACE=<a workspace you may create sandboxes in>
  make test-escape
  ```

  Every spec deletes the sandboxes, providers and profiles it creates. Kata isolation itself
  (the VM boundary, guest seccomp) is only exercised here, not in CI.
