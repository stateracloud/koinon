// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package guestfront

import "log/slog"

// The guest is Linux. On another system the reaper has nothing to do.
func enableSubreaper(*slog.Logger) {}

func listZombieChildren() ([]zombie, error) { return nil, nil }
