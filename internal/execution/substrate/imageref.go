// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"fmt"
	"strings"
)

// RewriteImage rewrites the registry of an actor image reference to the image source of the node
// agent (ADR 0061, ADR 0120 section 6, issue #1170). It is the Go twin of the chart helper
// koinon.imageRef and follows its rules:
//
//   - The leading host is the first path element that holds "." or ":" (or is "localhost"). A
//     reference without one is on docker.io, and a bare "name" is docker.io/library/name. The names
//     index.docker.io and registry-1.docker.io count as docker.io. Host names are compared in lower case.
//   - mirrors[host] replaces that host when it has an entry. Otherwise registry replaces it when it
//     is set. Otherwise the reference is returned as it is.
//   - A reference that already starts with registry or a mirror value is returned as it is, so
//     nothing is prefixed twice.
//   - The path, the tag and the digest are kept. A digest still verifies through a pull-through
//     cache, and the node agent checks it, so the cache needs no trust.
//
// The result is part of the template hash: a changed mirror name re-hashes every template.
func RewriteImage(ref, registry string, mirrors map[string]string) string {
	targets := make([]string, 0, len(mirrors)+1)
	targets = append(targets, registry)
	for _, t := range mirrors {
		targets = append(targets, t)
	}
	for _, t := range targets {
		if t != "" && strings.HasPrefix(ref, strings.TrimSuffix(t, "/")+"/") {
			return ref
		}
	}
	parts := strings.Split(ref, "/")
	host, rest := "docker.io", ref
	switch {
	case len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || strings.EqualFold(parts[0], "localhost")):
		host = strings.ToLower(parts[0])
		if host == "index.docker.io" || host == "registry-1.docker.io" {
			host = "docker.io"
		}
		rest = strings.Join(parts[1:], "/")
		// Docker Hub has no API alias: docker.io/alpine is docker.io/library/alpine.
		if host == "docker.io" && !strings.Contains(rest, "/") {
			rest = "library/" + rest
		}
	case len(parts) == 1:
		rest = "library/" + ref
	}
	target := mirrors[host]
	if target == "" {
		target = registry
	}
	if target == "" {
		return ref
	}
	return fmt.Sprintf("%s/%s", strings.TrimSuffix(target, "/"), rest)
}
