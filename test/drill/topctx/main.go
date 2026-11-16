// SPDX-License-Identifier: Apache-2.0

// Command topctx prints the kube context of a drill topology file, read by the same loader the suite uses
// (drill.LoadTopology), so test/drill/run.sh needs no second YAML parser. It exits 2 on a usage error and 1
// when the file does not load or validate.
//
//	go run ./test/drill/topctx <topology.yaml>
package main

import (
	"fmt"
	"os"

	"github.com/stateracloud/koinon/test/drill"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: topctx <topology.yaml>")
		os.Exit(2)
	}
	// The run ID only expands {run} in names; the context never has it.
	t, err := drill.LoadTopology(os.Args[1], "topctx")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(t.Context)
}
