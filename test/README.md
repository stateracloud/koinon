# test

End-to-end tests (kind), flavor contract tests and the sandbox escape suite (ADR 0025).

- `flavors/`: flavor contract suite. Runs each flavor image in Docker as OpenShell would and drives
  it over ACP against the fake model server. Skips without Docker or without the image
  (`make test-flavors` builds it).
