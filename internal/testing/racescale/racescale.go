// SPDX-License-Identifier: Apache-2.0

// Package racescale stretches the wall-clock timeouts of specs that do real CPU work (backups,
// encryption, restores) when the race detector is on. The go-race job runs those suites with -race,
// which slows such work by roughly an order of magnitude; the work itself is unchanged, so only the
// time we allow for it grows. Use it on timeouts and Eventually windows, never on specs that test
// elapsed time itself.
package racescale

import "time"

// D returns d, multiplied by the race factor when the binary is built with -race.
func D(d time.Duration) time.Duration { return d * factor }
