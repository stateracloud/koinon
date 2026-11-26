// SPDX-License-Identifier: Apache-2.0

package modelgateway

import "io"

// EchoSeen feeds the chunks to a new echo watch for key and reports whether it found the key.
func EchoSeen(key string, chunks ...[]byte) bool {
	w := newEchoWatch(key)
	seen := false
	for _, c := range chunks {
		seen = w.feed(c) || seen
	}
	return seen
}

// EchoPass reads rc through the echo watch for key and returns the bytes it passed and how often it reported
// the key.
func EchoPass(key string, rc io.ReadCloser) ([]byte, int, error) {
	n := 0
	b := &echoBody{ReadCloser: rc, watch: newEchoWatch(key), seen: func() { n++ }}
	out, err := io.ReadAll(b)
	return out, n, err
}
