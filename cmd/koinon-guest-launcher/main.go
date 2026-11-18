// SPDX-License-Identifier: Apache-2.0

// Command koinon-guest-launcher hardens the guest it runs in and then execs the agent (ADR 0119,
// issue #1087). A flavor image uses it as the entrypoint wrapper on the substrate driver path only:
//
//	koinon-guest-launcher [--policy FILE] [--check] -- COMMAND [ARG...]
//
// It sets no_new_privs, drops to the policy's user when the guest allows it, reduces the
// capabilities to an allowlist, applies Landlock filesystem rules and a seccomp deny filter, and
// replaces itself with COMMAND. The policy holds no credentials. Exit 64 is a usage or policy error,
// 70 a control that the policy requires and the guest could not apply.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/stateracloud/koinon/internal/guestlaunch"
)

func main() {
	policyPath := flag.String("policy", "", "policy JSON file (default: the built-in policy)")
	check := flag.Bool("check", false, "validate the policy, print it and exit")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	p := guestlaunch.DefaultPolicy()
	if *policyPath != "" {
		f, err := os.Open(*policyPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "koinon-guest-launcher:", err)
			os.Exit(64)
		}
		p, err = guestlaunch.ParsePolicy(f)
		_ = f.Close()
		if err != nil {
			fmt.Fprintln(os.Stderr, "koinon-guest-launcher:", err)
			os.Exit(64)
		}
	}
	if *check {
		fmt.Printf("%+v\n", p)
		return
	}
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: koinon-guest-launcher [--policy FILE] [--check] -- COMMAND [ARG...]")
		os.Exit(64)
	}
	err := guestlaunch.Exec(p, flag.Args(), os.Environ(), log)
	fmt.Fprintln(os.Stderr, "koinon-guest-launcher:", err)
	os.Exit(70)
}
