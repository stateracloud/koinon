// SPDX-License-Identifier: Apache-2.0

package pricing

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"sync"

	"go.yaml.in/yaml/v3"
)

//go:embed default.yaml
var defaultYAML []byte

// List is a versioned set of entries: the default price list.
type List struct {
	// Version identifies the list; it changes whenever any entry does. Ledger entries and the
	// admin API cite "<version>#<entry id>".
	Version string  `json:"version" yaml:"version"`
	Entries []Entry `json:"entries" yaml:"entries"`
}

var versionRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}\.[0-9]{1,3}$`)

// ParseList reads and validates a list. Entry IDs are unique per valid_from.
func ParseList(b []byte) (List, []Resolved, error) {
	var l List
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&l); err != nil {
		return List{}, nil, fmt.Errorf("pricing: %w", err)
	}
	if !versionRE.MatchString(l.Version) {
		return List{}, nil, fmt.Errorf("pricing: version %q must look like 2026-10-24.1", l.Version)
	}
	var errs []error
	var rs []Resolved
	seen := map[string]bool{}
	for _, e := range l.Entries {
		key := e.ID + "@" + e.ValidFrom
		if seen[key] {
			errs = append(errs, fmt.Errorf("pricing: duplicate entry %s", key))
		}
		seen[key] = true
		r, err := e.Resolve(false)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		rs = append(rs, r)
	}
	return l, rs, errors.Join(errs...)
}

var (
	defOnce sync.Once
	defList List
	defRes  []Resolved
	defErr  error
)

// Default returns the embedded default list. It panics if the embedded file is invalid, which
// the package test rules out.
func Default() (List, []Resolved) {
	defOnce.Do(func() { defList, defRes, defErr = ParseList(defaultYAML) })
	if defErr != nil {
		panic(defErr)
	}
	return defList, defRes
}

// Ref names the entry in a ledger row: "default@<list version>#<id>".
func Ref(listVersion, id string) string { return "default@" + listVersion + "#" + id }
