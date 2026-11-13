// SPDX-License-Identifier: Apache-2.0

package runevidence

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// HostMasker replaces host names in exported text with stable pseudonyms (`host-1a2b3c4d.invalid`),
// the same one for every occurrence of a name within one export, because the replay compares strings
// across calls (docs/dev/real-model-tests.md). The pseudonym is a keyed hash: the key is made per
// export and never stored, so a pseudonym cannot be matched against a list of guessed names.
//
// It masks what reads as a host: the authority of a URL, an IPv4 address, and an IPv6 address and a dotted name whose
// last label is a host suffix (hostSuffixes) or any two-letter country code that is not a common file
// extension (`.sh`, `.py`, `.go` stay; a host named `x.sh` is a miss). A bare single-label name ("localhost", a pod name) has no dot
// and is not recognised; the converter's output is checked by `koinon-api run-evidence verify`
// against the names the operator lists.
type HostMasker struct {
	key []byte
}

// NewHostMasker returns a masker with a fresh random key.
func NewHostMasker() (*HostMasker, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return &HostMasker{key: key}, nil
}

// NewHostMaskerWithKey is for tests, which need the same pseudonyms twice.
func NewHostMaskerWithKey(key []byte) *HostMasker { return &HostMasker{key: key} }

var (
	// A URL's authority: scheme://[userinfo@]host[:port]. The host is the part replaced.
	urlHost = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)([^/\s"'<>?#@\\]*@)?(\[[0-9a-f:]+\]|[a-z0-9_]([a-z0-9_.-]*[a-z0-9_])?)`)
	ipv4    = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])\b`)
	// An IPv6 address outside a URL: three or more groups, or a group on each side of a "::".
	ipv6   = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{1,4}:){3,7}[0-9a-f]{1,4}\b|\b(?:[0-9a-f]{1,4}:){1,6}:[0-9a-f]{1,4}\b`)
	dotted = regexp.MustCompile(`(?i)\b(?:[a-z0-9_](?:[a-z0-9_-]{0,61}[a-z0-9_])?\.)+([a-z][a-z0-9-]{0,22}[a-z0-9]|[a-z])\b`)
)

// hostSuffixes are the last labels of a dotted name that is masked outside a URL. The list is of
// suffixes, not of file extensions, so `fmt.Println` and `values.yaml` stay as they are while
// `db.internal`, `api.example.com` and `pod.ns.svc.cluster.local` do not.
var hostSuffixes = map[string]bool{}

func init() {
	for _, e := range strings.Fields(`com net org io dev app cloud ai co info biz xyz online site tech eu us uk de fr nl ch se no fi
		dk es it pl cz at be ie jp cn in au ca br ru ch local localhost internal svc lan home corp intranet cluster test example
		invalid arpa onion gov edu mil int mobi name pro tel travel museum aero coop jobs asia cat bank shop store blog page link live
		news world today work team email host space website network systems solutions services group agency digital top club vip
		icu wang fun tools zone pub run lol ly tv me dev`) {
		hostSuffixes[e] = true
	}
}

// fileExtensions are two-letter last labels that are file extensions far more often than country
// codes; every other two-letter last label is read as a country code and masked.
var fileExtensions = map[string]bool{
	"py": true, "rs": true, "md": true, "go": true, "js": true, "sh": true, "ts": true, "cs": true, "gz": true, "rb": true,
	"pl": true, "kt": true, "hs": true, "ml": true, "jl": true, "el": true, "ex": true, "db": true, "sq": true, "pm": true,
	"mk": true, "lo": true, "so": true, "pb": true, "tf": true, "vm": true, "id": true, "as": true, "bz": true, "xz": true,
}

func isHostSuffix(last string) bool {
	if hostSuffixes[last] {
		return true
	}
	return len(last) == 2 && !fileExtensions[last]
}

// Name returns the pseudonym of a host name.
func (m *HostMasker) Name(host string) string {
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte(strings.ToLower(host)))
	return "host-" + hex.EncodeToString(mac.Sum(nil)[:4]) + ".invalid"
}

// Mask replaces every host name in s.
func (m *HostMasker) Mask(s string) string {
	if m == nil {
		return s
	}
	s = urlHost.ReplaceAllStringFunc(s, func(match string) string {
		sub := urlHost.FindStringSubmatch(match)
		return sub[1] + sub[2] + m.Name(sub[3])
	})
	s = ipv4.ReplaceAllStringFunc(s, m.Name)
	s = ipv6.ReplaceAllStringFunc(s, m.Name)
	return dotted.ReplaceAllStringFunc(s, func(match string) string {
		if strings.HasSuffix(match, ".invalid") && strings.HasPrefix(match, "host-") {
			return match
		}
		last := match[strings.LastIndexByte(match, '.')+1:]
		if !isHostSuffix(strings.ToLower(last)) {
			return match
		}
		return m.Name(match)
	})
}
