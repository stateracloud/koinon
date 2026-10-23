// SPDX-License-Identifier: Apache-2.0

// Command chartlint checks rendered manifests for list entries that the API server rejects
// but kubeconform's schemas do not: a pod spec that repeats a volume name, a container
// name, or, within one container, a volume mount name or path or an env name.
//
// Usage: go run ./hack/chartlint FILE...
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v3"
)

func main() {
	problems := 0
	for _, path := range os.Args[1:] {
		n, err := checkFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(2)
		}
		problems += n
	}
	if problems > 0 {
		fmt.Fprintf(os.Stderr, "%d duplicate list entries\n", problems)
		os.Exit(1)
	}
}

func checkFile(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	dec := yaml.NewDecoder(f)
	problems := 0
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return problems, nil
			}
			return problems, err
		}
		if doc == nil {
			continue
		}
		name := fmt.Sprintf("%s %v/%v", path, doc["kind"], dig(doc, "metadata", "name"))
		for _, spec := range podSpecs(doc) {
			problems += checkPodSpec(name, spec)
		}
	}
}

// podSpecs returns the pod specs in a workload object: a Pod's spec, a pod template's spec
// (Deployment, StatefulSet, DaemonSet, Job, ReplicaSet) or a CronJob's job template's.
func podSpecs(doc map[string]any) []map[string]any {
	var out []map[string]any
	for _, path := range [][]string{
		{"spec"},
		{"spec", "template", "spec"},
		{"spec", "jobTemplate", "spec", "template", "spec"},
	} {
		if spec, ok := dig(doc, path...).(map[string]any); ok {
			if _, ok := spec["containers"]; ok {
				out = append(out, spec)
			}
		}
	}
	return out
}

func checkPodSpec(name string, spec map[string]any) int {
	problems := dupes(name, "volume name", list(spec["volumes"]), "name")
	var containers []any
	containers = append(containers, list(spec["initContainers"])...)
	containers = append(containers, list(spec["containers"])...)
	problems += dupes(name, "container name", containers, "name")
	for _, c := range containers {
		cm, _ := c.(map[string]any)
		where := fmt.Sprintf("%s container %v", name, cm["name"])
		problems += dupes(where, "volume mount name", list(cm["volumeMounts"]), "name")
		problems += dupes(where, "volume mount path", list(cm["volumeMounts"]), "mountPath")
		problems += dupes(where, "env name", list(cm["env"]), "name")
	}
	return problems
}

func dupes(where, what string, items []any, key string) int {
	seen := map[any]bool{}
	problems := 0
	for _, it := range items {
		m, _ := it.(map[string]any)
		v, ok := m[key]
		if !ok {
			continue
		}
		if seen[v] {
			fmt.Fprintf(os.Stderr, "%s: duplicate %s %v\n", where, what, v)
			problems++
		}
		seen[v] = true
	}
	return problems
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}
