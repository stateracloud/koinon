// SPDX-License-Identifier: Apache-2.0

package adminapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Service Unavailable responses carry Retry-After so clients know when to try again (#529).
func TestServiceUnavailableSetsRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}
	w.WriteHeader(http.StatusServiceUnavailable)
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want 30", got)
	}
	if !w.wrote || w.status != http.StatusServiceUnavailable {
		t.Fatalf("recorder status=%d wrote=%v", w.status, w.wrote)
	}
}
