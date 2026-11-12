// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/pluginpb"
)

// Regenerate the goldens with: go test ./cmd/protoc-gen-koinon-doc -update. Never edit them by hand.
var update = flag.Bool("update", false, "rewrite the golden files")

func loc(comment string, path ...int32) *descriptorpb.SourceCodeInfo_Location {
	return &descriptorpb.SourceCodeInfo_Location{Path: path, Span: []int32{0, 0, 1}, LeadingComments: proto.String(comment)}
}

// path builds a SourceCodeInfo path from names, so that reordering the fixture cannot misplace a
// comment. One name is an enum, message or service; two names are a child of it: an enum value, a
// field, a nested message or a method.
func path(f *descriptorpb.FileDescriptorProto, names ...string) []int32 {
	for i, e := range f.EnumType {
		if e.GetName() != names[0] {
			continue
		}
		p := []int32{5, int32(i)}
		for j, v := range e.Value {
			if len(names) == 2 && v.GetName() == names[1] {
				return append(p, 2, int32(j))
			}
		}
		if len(names) == 1 {
			return p
		}
	}
	for i, m := range f.MessageType {
		if m.GetName() != names[0] {
			continue
		}
		p := []int32{4, int32(i)}
		if len(names) == 1 {
			return p
		}
		for j, fl := range m.Field {
			if fl.GetName() == names[1] {
				return append(p, 2, int32(j))
			}
		}
		for j, n := range m.NestedType {
			if n.GetName() == names[1] {
				return append(p, 3, int32(j))
			}
		}
	}
	for i, sv := range f.Service {
		if sv.GetName() != names[0] {
			continue
		}
		p := []int32{6, int32(i)}
		if len(names) == 1 {
			return p
		}
		for j, m := range sv.Method {
			if m.GetName() == names[1] {
				return append(p, 2, int32(j))
			}
		}
	}
	panic("fixture: no element " + strings.Join(names, "."))
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
	created := fd("created", 9, msg)
	created.TypeName = proto.String(".google.protobuf.Timestamp")
	old := fd("old", 8, str)
	old.Options = &descriptorpb.FieldOptions{Deprecated: proto.Bool(true)}

	demo := &descriptorpb.FileDescriptorProto{
		Name: proto.String("demo/v1/demo.proto"), Package: proto.String("demo.v1"), Syntax: proto.String("proto3"),
		Dependency: []string{"google/protobuf/timestamp.proto"},
		Options:    &descriptorpb.FileOptions{GoPackage: proto.String("example.com/demo/v1;demov1")},
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("Mode"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("MODE_UNSPECIFIED"), Number: proto.Int32(0)},
				{Name: proto.String("MODE_FAST"), Number: proto.Int32(1)},
			},
		}},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name:  proto.String("HelloRequest"),
				Field: []*descriptorpb.FieldDescriptorProto{old, fd("name", 1, str), tags, labels, mode, byID, byName, note, created},
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
			{Name: proto.String("HelloReply")},
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
	}
	demo.SourceCodeInfo = &descriptorpb.SourceCodeInfo{Location: []*descriptorpb.SourceCodeInfo_Location{
		loc(" Mode picks the speed.\n It has a | pipe.\n", path(demo, "Mode")...),
		loc(" The fast mode.\n", path(demo, "Mode", "MODE_FAST")...),
		loc(" Request to greet.\n", path(demo, "HelloRequest")...),
		{
			Path: path(demo, "HelloRequest"), Span: []int32{0, 0, 1},
			LeadingDetachedComments: []string{" DETACHED must not appear.\n"},
		},
		loc(" Who to greet, as org#<id> or `org#<id>`.\n", path(demo, "HelloRequest", "name")...),
		loc(" Nested type.\n", path(demo, "HelloRequest", "Inner")...),
		loc(" Greets people.\n", path(demo, "Greeter")...),
		loc(" Says hello.\n", path(demo, "Greeter", "Hello")...),
	}}

	alpha := &descriptorpb.FileDescriptorProto{
		Name: proto.String("alpha/v1/alpha.proto"), Package: proto.String("alpha.v1"), Syntax: proto.String("proto3"),
		Options: &descriptorpb.FileOptions{GoPackage: proto.String("example.com/alpha/v1;alphav1")},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Empty")},
		},
	}
	return &pluginpb.CodeGeneratorRequest{
		FileToGenerate: []string{"demo/v1/demo.proto", "alpha/v1/alpha.proto"},
		ProtoFile:      []*descriptorpb.FileDescriptorProto{protodesc.ToFileDescriptorProto(timestamppb.File_google_protobuf_timestamp_proto), demo, alpha},
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
	// The comments must land on the intended elements, not only match the golden.
	for _, want := range []string{
		"Request to greet.", "Nested type.", "Who to greet, as org#&lt;id&gt; or `org#<id>`.",
	} {
		if !strings.Contains(string(md), want) {
			t.Errorf("markdown lacks %q", want)
		}
	}
	var doc struct {
		Packages []struct {
			Messages []struct {
				Name, Description string
				Fields            []struct{ Name, Description string }
			}
		}
	}
	if err := json.Unmarshal(js, &doc); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range doc.Packages {
		for _, m := range p.Messages {
			got[m.Name] = m.Description
			for _, f := range m.Fields {
				got[m.Name+"."+f.Name] = f.Description
			}
		}
	}
	for k, v := range map[string]string{
		"HelloRequest": "Request to greet.", "HelloReply": "", "HelloRequest.Inner": "Nested type.",
		"HelloRequest.name": "Who to greet, as org#<id> or `org#<id>`.",
	} {
		if got[k] != v {
			t.Errorf("description of %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestDeterministic(t *testing.T) {
	js1, md1 := run(t)
	js2, md2 := run(t)
	if string(js1) != string(js2) || string(md1) != string(md2) {
		t.Fatal("two runs gave different bytes")
	}
}
