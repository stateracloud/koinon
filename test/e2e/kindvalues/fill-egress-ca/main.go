// SPDX-License-Identifier: Apache-2.0

// Command fill-egress-ca writes the kind e2e values file with its egress CA placeholder filled in
// (kindvalues.FillEgressCA). test/e2e/run.sh calls it; the restore drill calls the function directly.
//
//	go run ./test/e2e/kindvalues/fill-egress-ca -values values.yaml -ca ca.crt
//
// rewrites values.yaml in place. It fails when the CA holds no certificate, and when the file holds no
// placeholder line unless -if-present is set (test/e2e/upgrade.sh: the values file of an older tag may have none).
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/stateracloud/koinon/test/e2e/kindvalues"
)

func main() {
	values := flag.String("values", "", "the values file to rewrite in place")
	ca := flag.String("ca", "", "the PEM file of the egress CA")
	ifPresent := flag.Bool("if-present", false, "leave the file as it is when it holds no placeholder line")
	flag.Parse()
	if err := run(*values, *ca, *ifPresent); err != nil {
		fmt.Fprintln(os.Stderr, "fill-egress-ca:", err)
		os.Exit(1)
	}
}

func run(values, ca string, ifPresent bool) error {
	if values == "" || ca == "" {
		return fmt.Errorf("-values and -ca are required")
	}
	v, err := os.ReadFile(values) //nolint:gosec // a test tool: the path is the caller's own file
	if err != nil {
		return err
	}
	c, err := os.ReadFile(ca) //nolint:gosec // a test tool: the path is the caller's own file
	if err != nil {
		return err
	}
	out, err := kindvalues.FillEgressCA(v, c)
	if ifPresent && errors.Is(err, kindvalues.ErrNoEgressCAPlaceholder) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", values, err)
	}
	return os.WriteFile(values, out, 0o600) // #nosec G703 -- a test tool: the path is the caller's own file
}
