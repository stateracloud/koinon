# Agent Substrate protos (vendored)

The Agent Substrate control API and the in-guest daemon API, used to generate Koinon's gRPC clients
in `internal/gen/substrate` (epic 1082, ADR 0119). Koinon does not import `go.ate.dev/substrate`:
the module pulls Substrate's whole dependency tree, and the guest proto sits in an `internal/`
package there.

- Upstream: https://github.com/agent-substrate/substrate
- Tag: `v1.0.0`, commit `b34fbee742906494b5eb4f1f36987176bf8770a4` (2026-11-05)
- Licence: Apache-2.0, Copyright The Agent Substrate Authors; see [LICENSE](LICENSE). The files keep
  their upstream headers.
- Files, copied unmodified:
  - `proto/ateapi.proto` from `api/ateapi/v1/ateapi.proto`: the `Control` service (actors,
    templates, atespaces, tags, egress policies, actor tokens).
  - `proto/guest.proto` from `internal/proto/ateenv/v1alpha/guest.proto`: `ProcessService` and
    `FileSystemService` served inside the actor. Substrate copied it from
    https://github.com/agent-substrate/env (`proto/ateenv/v1alpha/guest.proto`, commit
    `1c08a7a578afd896373511a83beb242aec1425c0`). The worker, atelet and object-store protos are not
    needed by a client.

`buf.gen.substrate.yaml` replaces the upstream `go_package` options with `M` options. The files are
not part of the buf workspace in `buf.yaml`, so `buf lint` and `buf breaking` check only Koinon's
own protos.

## Updating

1. Copy the two files from the new tag, unmodified, and update the tag and commit above.
2. Run `make generate`, then `go test ./internal/execution/...`.
3. Check the upstream changes to the RPCs `internal/execution/substrate` uses and to the behaviour
   `internal/execution/substrate/fakes` imitates (state names, the guest's output offsets, the
   rule that a router request resumes a suspended actor).
