# OpenShell protos (vendored)

The OpenShell gateway API, used to generate Koinon's gRPC client in `internal/gen/openshell`
(ADR 0048: generated client, not `sdk/go`).

- Upstream: https://github.com/NVIDIA/OpenShell
- Tag: `v0.1.7`, commit `5d25211b25a8fc887e8d5c606d57d6ab22f1d76c` (2026-11-03); v0.1.5 was
  `bdb956f2cd65` (2026-10-20), the commit the spike's SDK pin used
- Licence: Apache-2.0, copyright NVIDIA CORPORATION & AFFILIATES; see [LICENSE](LICENSE). The
  files keep their upstream SPDX headers.
- Files: `proto/openshell.proto`, `proto/datamodel.proto`, `proto/sandbox.proto` and
  `proto/options.proto`, copied unmodified from upstream `proto/`. These four are the
  `openshell.v1.OpenShell` service and its imports; the driver, extension and supervisor protos
  are not needed by a client.

The files have no `go_package` option; `buf.gen.openshell.yaml` assigns Go packages with `M`
options. They are not part of the buf workspace in `buf.yaml`, so `buf lint` and `buf breaking`
check only Koinon's own protos.

## Updating

1. Copy the four files from the new tag's `proto/` directory, unmodified, and update the tag and
   commit above.
2. Run `make generate`, then `go test ./internal/execution/...`.
3. Check the upstream changes to the RPCs `internal/execution/openshell` uses and to the semantics
   `internal/execution/openshell/fakegateway` imitates.
