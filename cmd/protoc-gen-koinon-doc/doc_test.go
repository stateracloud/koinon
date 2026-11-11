// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

// Regenerate the goldens with: go test ./cmd/protoc-gen-koinon-doc -update. Never edit them by hand.
var update = flag.Bool("update", false, "rewrite the golden files")

func loc(comment string, path ...int32) *descriptorpb.SourceCodeInfo_Location {
	return &descriptorpb.SourceCodeInfo_Location{Path: path, Span: []int32{0, 0, 1}, LeadingComments: proto.String(comment)}
}

func fd(name string, num int32, t descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name: proto.String(name), Number: proto.Int32(num), Type: t.Enum(),
		Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
	}
}

// fixture returns two files in two packages, listed out of order on purpose, to cover services,
// streaming, nesting, maps, repeated, proto3 optional, oneofs, enums, deprecation and comments.
func fixture() *pluginpb.CodeGeneratorRequest {
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	enm := descriptorpb.FieldDescriptorProto_TYPE_ENUM

	tags := fd("tags", 2, str)
	tags.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	labels := fd("labels", 3, msg)
	labels.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	labels.TypeName = proto.String(".demo.v1.HelloRequest.LabelsEntry")
	mode := fd("mode", 4, enm)
	mode.TypeName = proto.String(".demo.v1.Mode")
	note := fd("note", 5, str)
	note.Proto3Optional = proto.Bool(true)
	note.OneofIndex = proto.Int32(1)
	byID := fd("by_id", 6, str)
	byID.OneofIndex = proto.Int32(0)
	byName := fd("by_name", 7, msg)
	byName.TypeName = proto.String(".demo.v1.HelloRequest.Inner")
	byName.OneofIndex = proto.Int32(0)
	old := fd("old", 8, str)
	old.Options = &descriptorpb.FieldOptions{Deprecated: proto.Bool(true)}

	demo := &descriptorpb.FileDescriptorProto{
		Name: proto.String("demo/v1/demo.proto"), Package: proto.String("demo.v1"), Syntax: proto.String("proto3"),
		Options: &descriptorpb.FileOptions{GoPackage: proto.String("example.com/demo/v1;demov1")},
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("Mode"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("MODE_UNSPECIFIED"), Number: proto.Int32(0)},
				{Name: proto.String("MODE_FAST"), Number: proto.Int32(1)},
			},
		}},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("HelloReply")},
			{
				Name:  proto.String("HelloRequest"),
				Field: []*descriptorpb.FieldDescriptorProto{old, fd("name", 1, str), tags, labels, mode, byID, byName, note},
				OneofDecl: []*descriptorpb.OneofDescriptorProto{
					{Name: proto.String("target")}, {Name: proto.String("_note")},
				},
				NestedType: []*descriptorpb.DescriptorProto{
					{Name: proto.String("Inner"), Field: []*descriptorpb.FieldDescriptorProto{fd("id", 1, str)}},
					{
						Name: proto.String("LabelsEntry"), Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
						Field: []*descriptorpb.FieldDescriptorProto{fd("key", 1, str), fd("value", 2, str)},
					},
				},
			},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("Greeter"),
			Method: []*descriptorpb.MethodDescriptorProto{
				{
					Name: proto.String("Watch"), InputType: proto.String(".demo.v1.HelloRequest"),
					OutputType: proto.String(".demo.v1.HelloReply"), ServerStreaming: proto.Bool(true),
				},
				{
					Name: proto.String("Hello"), InputType: proto.String(".demo.v1.HelloRequest"),
					OutputType: proto.String(".demo.v1.HelloReply"),
					Options:    &descriptorpb.MethodOptions{Deprecated: proto.Bool(true)},
				},
			},
		}},
		SourceCodeInfo: &descriptorpb.SourceCodeInfo{Location: []*descriptorpb.SourceCodeInfo_Location{
			loc(" Mode picks the speed.\n It has a | pipe.\n", 5, 0),
			loc(" The fast mode.\n", 5, 0, 2, 1),
			loc(" Request to greet.\n", 4, 1),
			loc(" Who to greet.\n", 4, 1, 2, 2),
			loc(" Nested type.\n", 4, 1, 3, 0),
			loc(" Greets people.\n", 6, 0),
			loc(" Says hello.\n", 6, 0, 2, 1),
		}},
	}
	alpha := &descriptorpb.FileDescriptorProto{
		Name: proto.String("alpha/v1/alpha.proto"), Package: proto.String("alpha.v1"), Syntax: proto.String("proto3"),
		Options: &descriptorpb.FileOptions{GoPackage: proto.String("example.com/alpha/v1;alphav1")},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Empty")},
		},
	}
	return &pluginpb.CodeGeneratorRequest{
		FileToGenerate: []string{"demo/v1/demo.proto", "alpha/v1/alpha.proto"},
		ProtoFile:      []*descriptorpb.FileDescriptorProto{demo, alpha},
	}
}

func run(t *testing.T) (js, md []byte) {
	t.Helper()
	gen, err := protogen.Options{}.New(fixture())
	if err != nil {
		t.Fatal(err)
	}
	if err := generate(gen); err != nil {
		t.Fatal(err)
	}
	resp := gen.Response()
	if resp.GetError() != "" {
		t.Fatal(resp.GetError())
	}
	got := map[string]string{}
	for _, f := range resp.File {
		got[f.GetName()] = f.GetContent()
	}
	if len(got) != 2 {
		t.Fatalf("want 2 files, got %d", len(got))
	}
	return []byte(got[jsonName]), []byte(got[markdownName])
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(got) {
		t.Errorf("%s differs from the generator output; run: go test ./cmd/protoc-gen-koinon-doc -update", name)
	}
}

func TestGolden(t *testing.T) {
	js, md := run(t)
	golden(t, "proto-api.golden.json", js)
	golden(t, "proto-api.golden.md", md)
}

func TestDeterministic(t *testing.T) {
	js1, md1 := run(t)
	js2, md2 := run(t)
	if string(js1) != string(js2) || string(md1) != string(md2) {
		t.Fatal("two runs gave different bytes")
	}
}
