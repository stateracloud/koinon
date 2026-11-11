# Reference

`proto/proto-api.md` and `proto/proto-api.json` are the API reference of every message and service in
`api/proto`. `make generate` writes them (`buf.gen.docs.yaml`, plugin `protoc-gen-doc` pinned in `go.mod`);
CI's "no diff after make generate" check fails when they are stale. Do not edit them by hand.

koinon-docs (`stateracloud/koinon-docs`, issue 6) reads the two files at the release tag a docs build names,
for example `https://raw.githubusercontent.com/stateracloud/koinon/<tag>/docs/reference/proto/proto-api.json`.
The JSON is the input for generated pages; the Markdown is the same content for reading in the repository.
