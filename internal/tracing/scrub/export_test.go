// SPDX-License-Identifier: Apache-2.0

package scrub

// Counting returns a copy of s that adds the length of every text its detectors read to *n, so a
// test can bound the work a scrub does without timing it.
func Counting(s *Scrubber, n *int) *Scrubber {
	c := *s
	c.work = func(l int) { *n += l }
	return &c
}
