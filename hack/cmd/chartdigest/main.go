// SPDX-License-Identifier: Apache-2.0

// Command chartdigest writes an image digest into harnesses.opencode.image.digest of a chart's
// values.yaml (issue 1360). With execution.driver=substrate the reconciler refuses to start
// without a harness image pinned by digest (issue 1192), and the release builds the harness image
// after it has the chart sources, so the release job that packages the chart runs this tool on its
// checkout first. Nothing is committed.
//
// The tool edits the one scalar in place: it finds the YAML node, then replaces only that token on
// its line, so every comment, blank line and indentation of the file stays as it was. It fails when
// the key is missing or not a scalar, or when the digest is not sha256:<64 lower-case hex digits>.
//
// Usage: go run ./hack/cmd/chartdigest -values charts/koinon/values.yaml -digest sha256:<64 hex>
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// keyPath is the digest of the default harness, the one the Substrate driver needs.
var keyPath = []string{"harnesses", "opencode", "image", "digest"}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func main() {
	values := flag.String("values", "", "the chart's values.yaml to edit")
	digest := flag.String("digest", "", "the digest to write: sha256:<64 hex digits>")
	flag.Parse()
	if *values == "" || *digest == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: chartdigest -values FILE -digest sha256:<64 hex>")
		os.Exit(2)
	}
	in, err := os.ReadFile(*values) // #nosec G304 G703 -- the release job passes its own chart path
	if err != nil {
		fmt.Fprintf(os.Stderr, "chartdigest: %v\n", err)
		os.Exit(1)
	}
	out, err := SetDigest(in, *digest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chartdigest: %s: %v\n", *values, err)
		os.Exit(1)
	}
	if err := os.WriteFile(*values, out, 0o644); err != nil { // #nosec G306 G703 -- same mode as a checked-out values file
		fmt.Fprintf(os.Stderr, "chartdigest: %v\n", err)
		os.Exit(1)
	}
}

// SetDigest returns values with harnesses.opencode.image.digest set to digest. Only that scalar
// changes.
func SetDigest(values []byte, digest string) ([]byte, error) {
	if !digestRE.MatchString(digest) {
		return nil, fmt.Errorf("digest %q is not sha256:<64 lower-case hex digits>", digest)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(values, &doc); err != nil {
		return nil, fmt.Errorf("not valid YAML: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, errors.New("empty document")
	}
	node := doc.Content[0]
	for _, k := range keyPath {
		node = mapValue(node, k)
		if node == nil {
			return nil, fmt.Errorf("key %s is missing", strings.Join(keyPath, "."))
		}
	}
	if node.Kind != yaml.ScalarNode {
		return nil, fmt.Errorf("%s is not a scalar", strings.Join(keyPath, "."))
	}
	if node.Style == 0 && node.Value == "" {
		return nil, fmt.Errorf("%s has no value on its line; write digest: \"\"", strings.Join(keyPath, "."))
	}
	lines := bytes.SplitAfter(values, []byte("\n"))
	if node.Line < 1 || node.Line > len(lines) {
		return nil, errors.New("value position out of range")
	}
	line := lines[node.Line-1]
	start := node.Column - 1 // columns count runes; a values file line before the value is ASCII
	end, err := tokenEnd(line, start, node.Style)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.Join(keyPath, "."), err)
	}
	var b bytes.Buffer
	for i, l := range lines {
		if i != node.Line-1 {
			b.Write(l)
			continue
		}
		b.Write(l[:start])
		b.WriteString(`"` + digest + `"`)
		b.Write(l[end:])
	}
	return b.Bytes(), nil
}

// mapValue returns the value of key in a mapping node, or nil.
func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// tokenEnd returns the byte offset after the scalar token that starts at start in line.
func tokenEnd(line []byte, start int, style yaml.Style) (int, error) {
	if start < 0 || start >= len(line) {
		return 0, errors.New("value position out of range")
	}
	switch {
	case style&yaml.DoubleQuotedStyle != 0, style&yaml.SingleQuotedStyle != 0:
		q := line[start]
		for i := start + 1; i < len(line); i++ {
			if q == '"' && line[i] == '\\' {
				i++
				continue
			}
			if line[i] == q {
				if q == '\'' && i+1 < len(line) && line[i+1] == '\'' {
					i++
					continue
				}
				return i + 1, nil
			}
		}
		return 0, errors.New("a quoted value that spans lines is not supported")
	case style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0:
		return 0, errors.New("a block scalar is not supported")
	}
	// Plain scalar: it ends at a " #" comment or at the end of the line.
	end := len(line)
	if i := bytes.Index(line[start:], []byte(" #")); i >= 0 {
		end = start + i
	}
	return len(bytes.TrimRight(line[:end], " \t\r\n")), nil
}
