// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package guestlaunch

import (
	"errors"
	"log/slog"
)

// Exec is Linux only: Landlock, seccomp and capabilities are Linux kernel features.
func Exec(Policy, []string, []string, *slog.Logger) error {
	return errors.New("guest launcher: Linux only")
}
