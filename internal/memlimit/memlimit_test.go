// SPDX-License-Identifier: Apache-2.0

package memlimit

import (
	"os"
	"path/filepath"
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

func TestLimitTakesTheSmallerOfTheEnvironmentAndTheCgroup(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldFiles, oldInfo := cgroupFiles, meminfoPath
	defer func() { cgroupFiles, meminfoPath = oldFiles, oldInfo }()
	meminfoPath = write("meminfo", "MemTotal:       16000000 kB\nMemFree: 1 kB\n") // 16,384,000,000 bytes

	for _, c := range []struct {
		name, env, cgroup string
		want              int64
	}{
		{"no limit anywhere", "", "max\n", 0},
		{"the node's allocatable in the env, no cgroup limit", "15000000000", "max\n", 0},
		{"the cgroup is lower than the env", "15000000000", "536870912\n", 536870912},
		{"the env is lower than the cgroup", "268435456", "536870912\n", 268435456},
		{"only a cgroup limit", "", "536870912\n", 536870912},
		{"v1 'no limit' number is at or above the host", "", "9223372036854771712\n", 0},
		{"a cgroup at the host's memory is no limit", "", "16384000000\n", 0},
		{"an unparsable cgroup falls back to the env", "300000000", "junk\n", 300000000},
	} {
		t.Setenv(Env, c.env)
		cgroupFiles = []string{write("memory.max", c.cgroup)}
		if got := Limit(); got != c.want {
			t.Errorf("%s: Limit() = %d, want %d", c.name, got, c.want)
		}
	}
	cgroupFiles = []string{filepath.Join(dir, "absent")}
	t.Setenv(Env, "")
	if got := Limit(); got != 0 {
		t.Errorf("no files, no env: %d", got)
	}
	t.Setenv(Env, "123456789")
	if got := Limit(); got != 123456789 {
		t.Errorf("no cgroup to read: the env counts, got %d", got)
	}
}
