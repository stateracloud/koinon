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
  it over ACP against the fake model server. Skips without Docker or without the image
  (`make test-flavors` builds it).
- `escape/`: sandbox escape suite (ADR 0025, plan item E2), against a real OpenShell gateway only
  (`KOINON_OPENSHELL_ENDPOINT`, see the package comment); skipped otherwise. The GitHub read grant
  cases check that push, REST writes and repositories outside the binding are refused by policy.
