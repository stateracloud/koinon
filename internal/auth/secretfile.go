// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"os"
	"strings"
)

// ReadSecretFile reads a secret mounted as a file, such as a Kubernetes Secret
// key. It strips trailing CR and LF characters, which editors and `echo` add,
// and nothing else: other whitespace is part of the secret. An empty path
// yields an empty secret.
func ReadSecretFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // path is operator configuration
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
