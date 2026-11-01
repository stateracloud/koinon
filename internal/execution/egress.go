// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"fmt"
	"strings"
)

// CheckEgressHost refuses an egress host that OpenShell would read as a pattern. OpenShell
// matches a wildcard host rule (`*.example.com`, `**`, any glob) by name, and a wildcard that
// admits attacker-chosen labels reopens a DNS channel: the sandbox resolves
// `<secret>.attacker.example` and the lookup itself leaks the data, with no connection made. The
// vm and hardened isolation tiers refuse wildcards (ADR 0075, decision 13). No Koinon feature
// needs one, so the check has no exemption today.
//
// The error is safe to show to a user: it names the host and says what to write instead.
func CheckEgressHost(host string) error {
	if strings.ContainsAny(host, "*?[]{}") {
		return fmt.Errorf("the egress host %q is a pattern; wildcard hosts are refused because they reopen a DNS exfiltration channel, so name each host exactly", host)
	}
	return nil
}
