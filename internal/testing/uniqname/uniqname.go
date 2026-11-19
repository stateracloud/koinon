// SPDX-License-Identifier: Apache-2.0

// Package uniqname makes names that are unique among the processes that share one machine, one
// Docker daemon or one OpenShell gateway. A clock is not enough: two processes (or two specs) can read
// the same millisecond or nanosecond tick, and the same name then collides in Docker, in a gateway or
// in a database. A name here is built from the process ID and a counter of the process, so two names of
// one process differ by the counter and two names of different live processes differ by the ID.
package uniqname

import (
	"fmt"
	"os"
	"sync/atomic"
)

// span is the number of values the counter takes before it wraps. One test process never makes this
// many names for one resource kind, and the number stays short for gateways with a name length limit.
const span = 10_000

var counter atomic.Uint64

// Suffix returns a number that is unique among live processes of one machine: the process ID times
// span plus the next counter value. It has at most 11 digits (a process ID has at most 7), so a short
// prefix and the number fit in a 19-character OpenShell sandbox name.
func Suffix() int64 {
	return int64(os.Getpid())*span + int64((counter.Add(1)-1)%span)
}

// Name returns prefix, a dash and Suffix: "kesa-12345678".
func Name(prefix string) string { return fmt.Sprintf("%s-%d", prefix, Suffix()) }
