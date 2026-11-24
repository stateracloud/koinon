// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/stateracloud/koinon/internal/gateway/route"
)

// maxRoutesFile bounds the platform route file.
const maxRoutesFile = 1 << 20

// fileRoute is one route of the platform route file. The names are fixed; an unknown field is an
// error, so a typo cannot drop a rule.
type fileRoute struct {
	Name         string   `json:"name"`
	Class        string   `json:"class"`
	Host         string   `json:"host"`
	Port         int      `json:"port,omitempty"`
	Methods      []string `json:"methods"`
	PathPrefixes []string `json:"pathPrefixes"`
	QueryKeys    []string `json:"queryKeys,omitempty"`
}

type routesFile struct {
	Routes []fileRoute `json:"routes"`
}

// LoadPlatformRoutes reads the platform routes of file: the routes every org may use, such as the
// default registry list of ADR 0113 decision 5. The file has no org field and no credential field: a
// platform route carries no credential (package route refuses one), and an org's routes come from
// Koinon state (part B), never from a file of the install.
//
// Any fault refuses the whole file, so a rule that fails to load closes every route (ADR 0120 2.2).
func LoadPlatformRoutes(file string) (*route.Table, error) {
	f, err := os.Open(file) //nolint:gosec // the operator's flag
	if err != nil {
		return nil, fmt.Errorf("egress: open the route file: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxRoutesFile+1))
	if err != nil {
		return nil, fmt.Errorf("egress: read the route file: %w", err)
	}
	if len(b) > maxRoutesFile {
		return nil, errors.New("egress: the route file is larger than 1 MiB")
	}
	return ParsePlatformRoutes(b)
}

// ParsePlatformRoutes is LoadPlatformRoutes for the content of the file.
func ParsePlatformRoutes(b []byte) (*route.Table, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var rf routesFile
	if err := dec.Decode(&rf); err != nil {
		return nil, fmt.Errorf("egress: the route file: %w", err)
	}
	if dec.More() {
		return nil, errors.New("egress: the route file holds more than one JSON value")
	}
	rs := make([]route.Route, 0, len(rf.Routes))
	for _, r := range rf.Routes {
		rs = append(rs, route.Route{
			Name: r.Name, Class: route.Class(r.Class), Host: r.Host, Port: r.Port,
			Methods: r.Methods, PathPrefixes: r.PathPrefixes, QueryKeys: r.QueryKeys,
		})
	}
	t, err := route.Compile(rs)
	if err != nil {
		return nil, fmt.Errorf("egress: the route file: %w", err)
	}
	return t, nil
}
