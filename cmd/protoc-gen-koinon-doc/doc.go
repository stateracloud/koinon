// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// schemaID names the JSON layout. Change it only with a breaking change to the layout; adding a key
// is not breaking (docs/reference/README.md).
const schemaID = "koinon-proto-api/v1"

type document struct {
	Schema   string    `json:"schema"`
	Packages []Package `json:"packages"`
}

// Package groups everything declared in one proto package.
type Package struct {
	Name     string    `json:"name"`
	Files    []string  `json:"files"`
	Services []Service `json:"services"`
	Messages []Message `json:"messages"`
	Enums    []Enum    `json:"enums"`
}

// Service is a gRPC/Connect service.
type Service struct {
	Name        string   `json:"name"`
	FullName    string   `json:"fullName"`
	File        string   `json:"file"`
	Description string   `json:"description"`
	Deprecated  bool     `json:"deprecated"`
	Methods     []Method `json:"methods"`
}

// Method is one rpc.
type Method struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	RequestType     string `json:"requestType"`
	ResponseType    string `json:"responseType"`
	ClientStreaming bool   `json:"clientStreaming"`
	ServerStreaming bool   `json:"serverStreaming"`
	Deprecated      bool   `json:"deprecated"`
}

// Message is a message type; nested messages are listed on their own with a dotted name.
type Message struct {
	Name        string  `json:"name"`
	FullName    string  `json:"fullName"`
	File        string  `json:"file"`
	Description string  `json:"description"`
	Deprecated  bool    `json:"deprecated"`
	Fields      []Field `json:"fields"`
}

// Field is one message field.
type Field struct {
	Name        string `json:"name"`
	Number      int32  `json:"number"`
	Type        string `json:"type"`
	Label       string `json:"label"`
	Oneof       string `json:"oneof"`
	Deprecated  bool   `json:"deprecated"`
	Description string `json:"description"`
}

// Enum is an enum type.
type Enum struct {
	Name        string      `json:"name"`
	FullName    string      `json:"fullName"`
	File        string      `json:"file"`
	Description string      `json:"description"`
	Deprecated  bool        `json:"deprecated"`
	Values      []EnumValue `json:"values"`
}

// EnumValue is one enum value.
type EnumValue struct {
	Name        string `json:"name"`
	Number      int32  `json:"number"`
	Deprecated  bool   `json:"deprecated"`
	Description string `json:"description"`
}

// build collects the documented items of files, sorted so two runs give the same bytes.
func build(files []*protogen.File) document {
	pkgs := map[string]*Package{}
	get := func(f *protogen.File) *Package {
		name := string(f.Desc.Package())
		p, ok := pkgs[name]
		if !ok {
			p = &Package{Name: name, Files: []string{}, Services: []Service{}, Messages: []Message{}, Enums: []Enum{}}
			pkgs[name] = p
		}
		return p
	}
	for _, f := range files {
		p := get(f)
		p.Files = append(p.Files, f.Desc.Path())
		for _, s := range f.Services {
			p.Services = append(p.Services, service(f, s))
		}
		for _, e := range f.Enums {
			p.Enums = append(p.Enums, enum(f, e))
		}
		var walk func(m *protogen.Message)
		walk = func(m *protogen.Message) {
			if m.Desc.IsMapEntry() {
				return
			}
			p.Messages = append(p.Messages, message(f, m))
			for _, e := range m.Enums {
				p.Enums = append(p.Enums, enum(f, e))
			}
			for _, n := range m.Messages {
				walk(n)
			}
		}
		for _, m := range f.Messages {
			walk(m)
		}
	}
	d := document{Schema: schemaID, Packages: []Package{}}
	for _, p := range pkgs {
		sort.Strings(p.Files)
		sort.Slice(p.Services, func(i, j int) bool { return p.Services[i].FullName < p.Services[j].FullName })
		sort.Slice(p.Messages, func(i, j int) bool { return p.Messages[i].FullName < p.Messages[j].FullName })
		sort.Slice(p.Enums, func(i, j int) bool { return p.Enums[i].FullName < p.Enums[j].FullName })
		d.Packages = append(d.Packages, *p)
	}
	sort.Slice(d.Packages, func(i, j int) bool { return d.Packages[i].Name < d.Packages[j].Name })
	return d
}

func service(f *protogen.File, s *protogen.Service) Service {
	out := Service{
		Name: string(s.Desc.Name()), FullName: string(s.Desc.FullName()), File: f.Desc.Path(),
		Description: comment(s.Comments), Deprecated: deprecated(s.Desc.Options()),
		Methods: []Method{},
	}
	for _, m := range s.Methods {
		out.Methods = append(out.Methods, Method{
			Name: string(m.Desc.Name()), Description: comment(m.Comments),
			RequestType: string(m.Input.Desc.FullName()), ResponseType: string(m.Output.Desc.FullName()),
			ClientStreaming: m.Desc.IsStreamingClient(), ServerStreaming: m.Desc.IsStreamingServer(),
			Deprecated: deprecated(m.Desc.Options()),
		})
	}
	sort.Slice(out.Methods, func(i, j int) bool { return out.Methods[i].Name < out.Methods[j].Name })
	return out
}

func message(f *protogen.File, m *protogen.Message) Message {
	name := strings.TrimPrefix(string(m.Desc.FullName()), string(f.Desc.Package())+".")
	out := Message{
		Name: name, FullName: string(m.Desc.FullName()), File: f.Desc.Path(),
		Description: comment(m.Comments), Deprecated: deprecated(m.Desc.Options()), Fields: []Field{},
	}
	for _, fd := range m.Fields {
		out.Fields = append(out.Fields, field(fd))
	}
	sort.SliceStable(out.Fields, func(i, j int) bool { return out.Fields[i].Number < out.Fields[j].Number })
	return out
}

func field(f *protogen.Field) Field {
	d := f.Desc
	out := Field{
		Name: string(d.Name()), Number: int32(d.Number()), Type: typeName(d), Label: "singular",
		Deprecated: deprecated(d.Options()), Description: comment(f.Comments),
	}
	switch {
	case d.IsMap():
		out.Label = "map"
	case d.IsList():
		out.Label = "repeated"
	case d.HasOptionalKeyword():
		out.Label = "optional"
	}
	if o := d.ContainingOneof(); o != nil && !o.IsSynthetic() {
		out.Oneof = string(o.Name())
	}
	return out
}

func typeName(d protoreflect.FieldDescriptor) string {
	if d.IsMap() {
		return fmt.Sprintf("map<%s, %s>", typeName(d.MapKey()), typeName(d.MapValue()))
	}
	switch d.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return string(d.Message().FullName())
	case protoreflect.EnumKind:
		return string(d.Enum().FullName())
	default:
		return d.Kind().String()
	}
}

func enum(f *protogen.File, e *protogen.Enum) Enum {
	name := strings.TrimPrefix(string(e.Desc.FullName()), string(f.Desc.Package())+".")
	out := Enum{
		Name: name, FullName: string(e.Desc.FullName()), File: f.Desc.Path(),
		Description: comment(e.Comments), Deprecated: deprecated(e.Desc.Options()), Values: []EnumValue{},
	}
	for _, v := range e.Values {
		out.Values = append(out.Values, EnumValue{
			Name: string(v.Desc.Name()), Number: int32(v.Desc.Number()),
			Deprecated: deprecated(v.Desc.Options()), Description: comment(v.Comments),
		})
	}
	sort.SliceStable(out.Values, func(i, j int) bool { return out.Values[i].Number < out.Values[j].Number })
	return out
}

// deprecated reads the deprecated option of any descriptor options message.
func deprecated(opts any) bool {
	type dep interface{ GetDeprecated() bool }
	o, ok := opts.(dep)
	return ok && o.GetDeprecated()
}

// comment returns the leading comment without the comment markers' shared leading space.
func comment(c protogen.CommentSet) string {
	lines := strings.Split(strings.TrimSpace(string(c.Leading)), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(strings.TrimPrefix(l, " "), " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func renderJSON(d document) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// cell makes text safe inside a Markdown table cell.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

func renderMarkdown(d document) []byte {
	var b strings.Builder
	b.WriteString("# Koinon proto API reference\n\n")
	b.WriteString("Generated by `make generate` (`cmd/protoc-gen-koinon-doc`). Do not edit by hand.\n\n")
	for _, p := range d.Packages {
		fmt.Fprintf(&b, "## %s\n\n", p.Name)
		fmt.Fprintf(&b, "Files: %s\n\n", "`"+strings.Join(p.Files, "`, `")+"`")
		for _, s := range p.Services {
			fmt.Fprintf(&b, "### Service %s\n\n", s.Name)
			if s.Deprecated {
				b.WriteString("Deprecated.\n\n")
			}
			if s.Description != "" {
				b.WriteString(s.Description + "\n\n")
			}
			b.WriteString("| Method | Request | Response | Streaming | Description |\n| --- | --- | --- | --- | --- |\n")
			for _, m := range s.Methods {
				stream := "-"
				switch {
				case m.ClientStreaming && m.ServerStreaming:
					stream = "bidirectional"
				case m.ClientStreaming:
					stream = "client"
				case m.ServerStreaming:
					stream = "server"
				}
				desc := cell(m.Description)
				if m.Deprecated {
					desc = strings.TrimSpace("Deprecated. " + desc)
				}
				fmt.Fprintf(&b, "| %s | `%s` | `%s` | %s | %s |\n", m.Name, m.RequestType, m.ResponseType, stream, desc)
			}
			b.WriteString("\n")
		}
		for _, m := range p.Messages {
			fmt.Fprintf(&b, "### Message %s\n\n", m.Name)
			if m.Deprecated {
				b.WriteString("Deprecated.\n\n")
			}
			if m.Description != "" {
				b.WriteString(m.Description + "\n\n")
			}
			if len(m.Fields) == 0 {
				b.WriteString("No fields.\n\n")
				continue
			}
			b.WriteString("| Field | Number | Type | Label | Oneof | Description |\n| --- | --- | --- | --- | --- | --- |\n")
			for _, f := range m.Fields {
				desc := cell(f.Description)
				if f.Deprecated {
					desc = strings.TrimSpace("Deprecated. " + desc)
				}
				fmt.Fprintf(&b, "| %s | %d | `%s` | %s | %s | %s |\n", f.Name, f.Number, f.Type, f.Label, f.Oneof, desc)
			}
			b.WriteString("\n")
		}
		for _, e := range p.Enums {
			fmt.Fprintf(&b, "### Enum %s\n\n", e.Name)
			if e.Deprecated {
				b.WriteString("Deprecated.\n\n")
			}
			if e.Description != "" {
				b.WriteString(e.Description + "\n\n")
			}
			b.WriteString("| Value | Number | Description |\n| --- | --- | --- |\n")
			for _, v := range e.Values {
				desc := cell(v.Description)
				if v.Deprecated {
					desc = strings.TrimSpace("Deprecated. " + desc)
				}
				fmt.Fprintf(&b, "| %s | %d | %s |\n", v.Name, v.Number, desc)
			}
			b.WriteString("\n")
		}
	}
	return []byte(strings.TrimRight(b.String(), "\n") + "\n")
}
