// SPDX-License-Identifier: Apache-2.0

// Command protoc-gen-koinon-doc is a buf/protoc plugin that writes the Koinon proto API reference:
// docs/reference/proto/proto-api.json (the schema documented in docs/reference/README.md) and
// proto-api.md (the same content as tables). It reads every file of one request, so run it with
// "strategy: all". The output is deterministic: everything is sorted, nothing depends on time or map
// order. It replaces protoc-gen-doc, which has been unmaintained since 2022 (issue 127).
package main

import (
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/types/pluginpb"
)

const (
	jsonName     = "proto-api.json"
	markdownName = "proto-api.md"
)

func main() {
	protogen.Options{}.Run(func(gen *protogen.Plugin) error {
		gen.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)
		return generate(gen)
	})
}

func generate(gen *protogen.Plugin) error {
	var files []*protogen.File
	for _, f := range gen.Files {
		if f.Generate {
			files = append(files, f)
		}
	}
	d := build(files)
	js, err := renderJSON(d)
	if err != nil {
		return err
	}
	if _, err := gen.NewGeneratedFile(jsonName, "").Write(js); err != nil {
		return err
	}
	_, err = gen.NewGeneratedFile(markdownName, "").Write(renderMarkdown(d))
	return err
}
