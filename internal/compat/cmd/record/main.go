// SPDX-License-Identifier: Apache-2.0

// Command record writes a persisted-state compatibility folder (internal/compat). Run it through
// hack/record-compat-fixtures.sh.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/stateracloud/koinon/internal/compat"
)

func main() {
	release := flag.String("release", "", "release name the folder is recorded for, e.g. v0.1.0")
	out := flag.String("out", "", "folder to write (must be empty or absent)")
	flag.Parse()
	if *release == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: record -release <name> -out <dir>")
		os.Exit(2)
	}
	if err := compat.Record(context.Background(), *out, *release); err != nil {
		fmt.Fprintln(os.Stderr, "record:", err)
		os.Exit(1)
	}
}
