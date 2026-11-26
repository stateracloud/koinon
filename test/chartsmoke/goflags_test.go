// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// A plain test, not a spec: it needs neither Docker nor envtest.
func TestEnvWithoutTestingTag(t *testing.T) {
	cases := map[string]string{
		"-tags=testing":                    "",
		"-tags=testing,webui":              "-tags=webui",
		"-race -tags=testing":              "-race",
		"'-tags=testing'":                  "",
		`"-tags=testing"`:                  "",
		"--tags=testing":                   "",
		"--tags=a,testing -race":           "-tags=a -race",
		`-tags="testing"`:                  "",
		"-tags=webui -mod=readonly":        "-tags=webui -mod=readonly",
		"-mod=readonly":                    "-mod=readonly",
		"'--tags=testing,webui' -trimpath": "-tags=webui -trimpath",
	}
	for in, want := range cases {
		got := envWithoutTestingTag([]string{"A=1", "GOFLAGS=" + in})
		if !slices.Equal(got, []string{"A=1", "GOFLAGS=" + want}) {
			t.Errorf("GOFLAGS=%q: got %q, want GOFLAGS=%q", in, got, want)
		}
	}
}

// A cancelled context, or a state read that failed, is "not checked": it must never read as a
// stopped Deployment (issue #1303).
func TestClassifyState(t *testing.T) {
	boom := errors.New("docker did not answer")
	cases := []struct {
		name            string
		running         bool
		readErr, ctxErr error
		want            runState
	}{
		{"running", true, nil, nil, stateRunning},
		{"stopped", false, nil, nil, stateStopped},
		{"cancelled context", false, context.Canceled, context.Canceled, stateNotChecked},
		{"deadline passed while the container ran", true, nil, context.DeadlineExceeded, stateNotChecked},
		{"deadline passed, container stopped", false, nil, context.DeadlineExceeded, stateNotChecked},
		{"state read failed", false, boom, nil, stateNotChecked},
	}
	for _, c := range cases {
		if got := classifyState(c.running, c.readErr, c.ctxErr); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
