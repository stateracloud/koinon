# test

End-to-end tests (kind), flavor contract tests and the sandbox escape suite (ADR 0025).

- `flavors/`: flavor contract suite. Runs each flavor image in Docker as OpenShell would and drives
  it over ACP against the fake model server. Skips without Docker or without the image
  (`make test-flavors` builds it).
- `escape/`: sandbox escape suite (ADR 0025, plan item E2), against a real OpenShell gateway only
  (`KOINON_OPENSHELL_ENDPOINT`, see the package comment); skipped otherwise. The GitHub read grant
  cases check that push, REST writes and repositories outside the binding are refused by policy.
