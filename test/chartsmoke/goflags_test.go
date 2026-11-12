// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
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
