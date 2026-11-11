# Reference

`proto/proto-api.md` and `proto/proto-api.json` are the API reference of every message, enum and service in
`api/proto`. `make generate` writes them (`buf.gen.docs.yaml`, plugin `cmd/protoc-gen-koinon-doc`, owned by
this repository); CI's "no diff after make generate" check fails when they are stale. Do not edit them by
hand. The output is deterministic: everything is sorted and nothing depends on time.

koinon-docs (`stateracloud/koinon-docs`, issue 6) reads the two files at the release tag a docs build names,
for example `https://raw.githubusercontent.com/stateracloud/koinon/<tag>/docs/reference/proto/proto-api.json`.
The JSON is the input for generated pages; the Markdown is the same content for reading in the repository.

## JSON schema (`koinon-proto-api/v1`)

Adding a key is not a breaking change; renaming, removing or retyping one is, and bumps the version in `schema`.
Every list is present (never `null`) and sorted as stated. Comments are the leading comments of the proto
declaration, with the comment markers removed.

```
{
  "schema": "koinon-proto-api/v1",
  "packages": [                       // sorted by name
    {
      "name": "koinon.auth.v1alpha1", // proto package
      "files": ["koinon/auth/v1alpha1/auth.proto"],   // sorted
      "services": [                   // sorted by fullName
        { "name", "fullName", "file", "description", "deprecated",
          "methods": [                // sorted by name
            { "name", "description", "requestType", "responseType",   // full names, no leading dot
              "clientStreaming", "serverStreaming", "deprecated" } ] } ],
      "messages": [                   // sorted by fullName; nested messages are listed on their own
        { "name",                     // name inside the package, "Outer.Inner" when nested
          "fullName", "file", "description", "deprecated",
          "fields": [                 // sorted by number
            { "name", "number",
              "type",                 // scalar name ("string", "int64"), a full message or enum name,
                                      // or "map<key, value>"
              "label",                // "singular" (implicit or explicit presence: message fields and oneof
                                      // members have explicit presence) | "optional" (only the proto3
                                      // `optional` keyword) | "repeated" | "map"
              "oneof",                // name of the real oneof, "" if none
              "deprecated", "description" } ] } ],
      "enums": [                      // sorted by fullName; nested enums are listed on their own
        { "name", "fullName", "file", "description", "deprecated",
          "values": [ { "name", "number", "deprecated", "description" } ] } ]   // sorted by number
    }
  ]
}
```

Map entry messages are not listed; a map field has `label` `map`. The generator's unit test
(`cmd/protoc-gen-koinon-doc`) holds a golden for both files; regenerate the goldens with
`go test ./cmd/protoc-gen-koinon-doc -update`, never by hand.
