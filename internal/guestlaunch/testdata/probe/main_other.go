// SPDX-License-Identifier: Apache-2.0

//go:build !linux

// Command probe only runs on Linux; this stub keeps the package loadable by tooling on other systems.
package main

func main() {}
