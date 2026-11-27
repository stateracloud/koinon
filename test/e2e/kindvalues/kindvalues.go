// SPDX-License-Identifier: Apache-2.0

// Package kindvalues fills the placeholders of the kind e2e values file (test/e2e/kind/koinon-values.yaml)
// that only a test run can know. The quickstart (test/e2e/run.sh, through the fill-egress-ca command) and the
// restore drill (test/drill) use the same function, so the two installs cannot drift apart (#1487).
package kindvalues

import (
	"bytes"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// EgressCAPlaceholder is the quoted value that marks the line of gateway.mcp.koinon.egress.caBundle.
const EgressCAPlaceholder = `"@E2E_EGRESS_CA@"`

// ErrNoEgressCAPlaceholder is returned when the values hold no EgressCAPlaceholder line.
var ErrNoEgressCAPlaceholder = errors.New("no " + EgressCAPlaceholder + " line in the values")

// FillEgressCA replaces the one line that holds EgressCAPlaceholder (for example `caBundle: "@E2E_EGRESS_CA@"`)
// with a YAML literal block that holds caPEM, indented two spaces below the key. caPEM must hold at least
// one PEM certificate. Values without the placeholder give ErrNoEgressCAPlaceholder; values with more than
// one placeholder line give an error.
func FillEgressCA(values, caPEM []byte) ([]byte, error) {
	if !holdsCertificate(caPEM) {
		return nil, errors.New("the egress CA holds no PEM certificate")
	}
	lines := strings.Split(string(values), "\n")
	var out []string
	found := 0
	for _, l := range lines {
		i := strings.Index(l, EgressCAPlaceholder)
		if i < 0 {
			out = append(out, l)
			continue
		}
		found++
		if strings.TrimSpace(l[i+len(EgressCAPlaceholder):]) != "" {
			return nil, fmt.Errorf("the %s line has text after the placeholder: %q", EgressCAPlaceholder, l)
		}
		indent := l[:len(l)-len(strings.TrimLeft(l, " "))]
		out = append(out, l[:i]+"|")
		for _, c := range strings.Split(strings.TrimRight(string(caPEM), "\n"), "\n") {
			out = append(out, indent+"  "+c)
		}
	}
	switch found {
	case 0:
		return nil, ErrNoEgressCAPlaceholder
	case 1:
		return []byte(strings.Join(out, "\n")), nil
	default:
		return nil, fmt.Errorf("%d %s lines in the values; want one", found, EgressCAPlaceholder)
	}
}

func holdsCertificate(b []byte) bool {
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return false
		}
		if blk.Type == "CERTIFICATE" && len(bytes.TrimSpace(blk.Bytes)) > 0 {
			return true
		}
	}
}
