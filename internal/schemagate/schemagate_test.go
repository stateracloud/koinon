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
	r := schemagate.Range{Min: 10, Max: 12}
	for v, want := range map[int64]string{9: "older", 10: "", 11: "", 12: "", 13: "newer", 0: "older"} {
		err := schemagate.Judge(v, r)
		switch {
		case want == "" && err != nil:
			t.Errorf("version %d: %v", v, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("version %d: got %v, want an error saying %q", v, err, want)
		}
	}
}

func TestGateFollowsTheSchema(t *testing.T) {
	var logs bytes.Buffer
	version, readErr := int64(11), error(nil)
	g := schemagate.NewFunc(func(context.Context) (int64, error) { return version, readErr }, schemagate.Range{Min: 10, Max: 12},
		slog.New(slog.NewTextHandler(&logs, nil)))

	if g.Ready() == nil {
		t.Fatal("ready before the first check")
	}
	status := func() int {
		rec := httptest.NewRecorder()
		g.ReadyHandler()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec.Code
	}
	if status() != http.StatusServiceUnavailable {
		t.Fatal("/readyz before the first check must be 503")
	}

	_ = g.Check(t.Context())
	if g.Ready() != nil || status() != http.StatusOK {
		t.Fatalf("version 11 in 10..12 must be ready: %v", g.Ready())
	}

	version = 13 // an older release after an app-only rollback
	if g.Check(t.Context()) == nil || g.Ready() == nil || status() != http.StatusServiceUnavailable {
		t.Fatal("a schema newer than the range must be not ready")
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "newer than the 12") {
		t.Fatalf("no clear log line: %s", logs.String())
	}
	logs.Reset()
	_ = g.Check(t.Context())
	if logs.Len() != 0 {
		t.Fatalf("an unchanged verdict must not log again: %s", logs.String())
	}

	version = 9 // a restore from an older backup
	if g.Check(t.Context()) == nil || !strings.Contains(g.Ready().Error(), "older") {
		t.Fatal("a schema older than the range must be not ready")
	}

	readErr = errors.New("connection refused")
	if g.Check(t.Context()) == nil || g.Ready() == nil {
		t.Fatal("an unreadable schema version must be not ready")
	}

	version, readErr = 12, nil
	if g.Check(t.Context()) != nil || g.Ready() != nil {
		t.Fatal("must recover when the schema is back in range")
	}
}
