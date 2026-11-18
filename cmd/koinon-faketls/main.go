// SPDX-License-Identifier: Apache-2.0

// Command koinon-faketls makes the throw-away CA and server certificate of the fake MCP server for one
// test run (e2e, drill). The run makes the set once and gives it to the fake as a Secret, so a restart of
// the pod keeps the same CA, and the MCP server record that trusts the CA stays valid. Nothing it writes
// is for production, and nothing is kept in the repository: ca.crt is public, tls.key goes into the Secret.
//
//	koinon-faketls -dir DIR -hosts fakemcp.ns.svc,localhost,127.0.0.1
//
// writes DIR/ca.crt, DIR/tls.crt and DIR/tls.key (mode 0600).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stateracloud/koinon/internal/faketls"
	"github.com/stateracloud/koinon/internal/project"
)

func main() {
	dir := flag.String("dir", "", "directory to write ca.crt, tls.crt and tls.key to (made if missing)")
	hosts := flag.String("hosts", "", "comma-separated host names and IPs of the server certificate")
	name := flag.String("name", project.Name+"-faketls test CA", "common name of the CA")
	flag.Parse()
	if err := run(*dir, *name, strings.Split(*hosts, ",")); err != nil {
		fmt.Fprintln(os.Stderr, "koinon-faketls:", err)
		os.Exit(1)
	}
}

func run(dir, name string, hosts []string) error {
	if dir == "" {
		return fmt.Errorf("-dir is required")
	}
	set, err := faketls.Make(name, hosts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for file, b := range map[string][]byte{"ca.crt": set.CA, "tls.crt": set.Cert, "tls.key": set.Key} {
		if err := os.WriteFile(filepath.Join(dir, file), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}
