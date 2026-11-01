// SPDX-License-Identifier: Apache-2.0

package schemagate_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/schemagate"
)

func TestJudge(t *testing.T) {
	r := schemagate.Range{Min: 10, Latest: 12}
	for name, c := range map[string]struct {
		s    schemagate.Schema
		want string
	}{
		"below min":          {schemagate.Schema{Version: 9, MinReader: 5}, "older"},
		"empty database":     {schemagate.Schema{}, "older"},
		"at min":             {schemagate.Schema{Version: 10}, ""},
		"newer expand-only":  {schemagate.Schema{Version: 15, MinReader: 11}, ""},
		"reader is latest":   {schemagate.Schema{Version: 14, MinReader: 12}, ""},
		"breaking migration": {schemagate.Schema{Version: 14, MinReader: 13}, "older release"},
	} {
		err := schemagate.Judge(c.s, r)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: got %v, want an error saying %q", name, err, c.want)
		}
	}
}

func TestGateFollowsTheSchema(t *testing.T) {
	var logs bytes.Buffer
	sc, readErr := schemagate.Schema{Version: 11, MinReader: 5}, error(nil)
	g := schemagate.NewFunc(func(context.Context) (schemagate.Schema, error) { return sc, readErr }, schemagate.Range{Min: 10, Latest: 12},
		slog.New(slog.NewTextHandler(&logs, nil)))

	if g.Ready() == nil {
		t.Fatal("ready before the first check")
	}
	status := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil)
		g.ReadyHandler()(rec, req)
		return rec.Code
	}
	if status() != http.StatusServiceUnavailable {
		t.Fatal("/readyz before the first check must be 503")
	}

	_ = g.Check(t.Context())
	if g.Ready() != nil || status() != http.StatusOK {
		t.Fatalf("a schema this build can read must be ready: %v", g.Ready())
	}

	sc = schemagate.Schema{Version: 20, MinReader: 11} // a later patch release added expand migrations
	if g.Check(t.Context()) != nil || g.Ready() != nil {
		t.Fatal("a newer expand-only schema must stay ready for the older release")
	}

	sc = schemagate.Schema{Version: 20, MinReader: 20} // a breaking migration in a new minor line
	if g.Check(t.Context()) == nil || g.Ready() == nil || status() != http.StatusServiceUnavailable {
		t.Fatal("a schema that declares a newer minimum reader must be not ready")
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "knows up to 12") {
		t.Fatalf("no clear log line: %s", logs.String())
	}
	logs.Reset()
	_ = g.Check(t.Context())
	if logs.Len() != 0 {
		t.Fatalf("an unchanged verdict must not log again: %s", logs.String())
	}

	sc = schemagate.Schema{Version: 9} // a restore from an older backup
	if g.Check(t.Context()) == nil || !strings.Contains(g.Ready().Error(), "older") {
		t.Fatal("a schema older than the minimum must be not ready")
	}

	readErr = errors.New("connection refused")
	if g.Check(t.Context()) == nil || g.Ready() == nil {
		t.Fatal("an unreadable schema must be not ready")
	}

	sc, readErr = schemagate.Schema{Version: 12}, nil
	if g.Check(t.Context()) != nil || g.Ready() != nil {
		t.Fatal("must recover when the schema is readable again")
	}
}
