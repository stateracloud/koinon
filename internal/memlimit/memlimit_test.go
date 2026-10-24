// SPDX-License-Identifier: Apache-2.0

package memlimit

import (
	"runtime/debug"
	"testing"
)

func TestApply(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(prev)
	for _, c := range []struct {
		env, gomem string
		want       int64
	}{
		{"536870912", "", 536870912 / 100 * 80},
		{"", "", 0},
		{"junk", "", 0},
		{"-5", "", 0},
		{"536870912", "1GiB", 0},
	} {
		t.Setenv(Env, c.env)
		t.Setenv("GOMEMLIMIT", c.gomem)
		if got := Apply(); got != c.want {
			t.Errorf("env %q GOMEMLIMIT %q: got %d, want %d", c.env, c.gomem, got, c.want)
		}
	}
}
