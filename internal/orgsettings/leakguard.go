// SPDX-License-Identifier: Apache-2.0

package orgsettings

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/leakguard"
	"github.com/stateracloud/koinon/internal/sinkclass"
)

// The org's leak-guard settings (ADR 0084 decisions 3 and 5, old issue 350): the match threshold and the
// fetch-trusted host list. The guard (old issue 349) reads them through LeakGuardOf and Service.LeakGuard;
// org admins change them through the Service, which audits every change.

// InvalidError is a validation failure; ErrNotFound says the host is not on the list.
type InvalidError = catalog.InvalidError

// ErrNotFound is returned for a host that is not on the org's fetch-trusted list.
var ErrNotFound = catalog.ErrNotFound

// DefaultMatchThreshold is the leak guard's match threshold when the org set none (ADR 0084 decision 3).
const DefaultMatchThreshold = leakguard.DefaultThreshold

// MaxTrustedHosts is the most hosts an org's fetch-trusted list holds. The list is an exemption
// from the URL rule; a long one is a sign the rule is being switched off by hand.
const MaxTrustedHosts = 256

// LeakGuard is the org's effective leak-guard configuration.
type LeakGuard struct {
	// MatchThreshold is the shortest tainted text, in characters after normalisation, that blocks a call.
	MatchThreshold int
	// TrustedHosts are the hosts the URL rule exempts, sorted. Empty for a new org.
	TrustedHosts []string
	// CodeHostRepos is the org's code-host scope with each repository's visibility (ADR 0091
	// decision 3). Empty for an org whose connectors have not reported one: then no code-host write
	// is internal.
	CodeHostRepos []sinkclass.Repo
}

// TrustsHost reports whether the URL rule exempts host. host is a URL's host name; case and a
// trailing dot are ignored. This is the contract the guard (old issue 349) relies on (ADR 0084):
//   - an entry "docs.example.com" trusts exactly that host and nothing else, not its subdomains and
//     not its parent;
//   - an entry "*.example.com" trusts every subdomain of example.com at any depth (a.example.com,
//     a.b.example.com) but not example.com itself, which needs its own entry;
//   - a match is on whole labels: "evilexample.com" and "example.com.evil.net" match neither form.
//
// A host that is not a DNS name (an IP address, a port, empty) is never trusted.
func (l LeakGuard) TrustsHost(host string) bool {
	h, err := NormalizeHost(host)
	if err != nil {
		return false
	}
	for _, t := range l.TrustedHosts {
		if rest, ok := strings.CutPrefix(t, "*"); ok {
			// rest is ".example.com": the dot keeps the match to whole labels, and the length test
			// keeps the apex out.
			if len(h) > len(rest) && strings.HasSuffix(h, rest) {
				return true
			}
			continue
		}
		if t == h {
			return true
		}
	}
	return false
}

// IsWildcard reports whether a stored entry is a "*.example.com" entry.
func IsWildcard(entry string) bool { return strings.HasPrefix(entry, "*.") }

// NormalizeTrustedEntry returns the stored form of a fetch-trusted entry: a host as NormalizeHost
// gives it, or "*." and such a host. Only a leading "*." is allowed, and the host after it follows the
// same rules as an exact entry, and must not be a public suffix (a wildcard over "com" or "co.uk"
// would trust the web).
func NormalizeTrustedEntry(raw string) (string, error) {
	t := strings.TrimSpace(raw)
	rest, wild := strings.CutPrefix(t, "*.")
	if !wild {
		return NormalizeHost(t)
	}
	if strings.Contains(rest, "*") {
		return "", &InvalidError{Msg: "a wildcard entry is *. followed by a host name, such as *.example.com; * is allowed only at the start"}
	}
	h, err := NormalizeHostLabels(rest)
	if err != nil {
		return "", err
	}
	if suffix, _ := publicsuffix.PublicSuffix(h); suffix == h {
		return "", &InvalidError{Msg: "*." + h + " would trust every host under the public suffix " + h + "; name a domain someone owns, such as *.example." + h}
	}
	return "*." + h, nil
}

var numericLabel = regexp.MustCompile(`^([0-9]+|0x[0-9a-f]*)$`)

var hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// NormalizeHost returns the lower-case form of a DNS host name, or an error when raw is not one: a
// URL, a host with a port, a path, a wildcard or an IP address is refused, so an entry names exactly
// one host. Also refused:
//   - a single label such as localhost or intranet: it resolves through the local search path, not
//     to a name anyone owns, so it cannot be trusted by name;
//   - a name whose last label is all digits or 0x followed by hex digits (127.1, 2130706433, 0x7f.1):
//     a resolver or URL parser reads it as an IPv4 address in a shorthand form (inet_aton, WHATWG URL);
//
// A Unicode name (bücher.example) is accepted and stored in its punycode form (xn--bcher-kva.example),
// the form a URL's host has when the guard compares it; a name that does not convert is refused.
func NormalizeHost(raw string) (string, error) {
	return NormalizeHostLabels(raw)
}

// NormalizeHostLabels is NormalizeHost; the wildcard entry form reuses it for the part after "*.".
func NormalizeHostLabels(raw string) (string, error) {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	for _, r := range h {
		if r > 0x7f {
			ascii, err := idna.Lookup.ToASCII(h)
			if err != nil {
				return "", &InvalidError{Msg: "host is not a valid internationalised domain name"}
			}
			h = ascii
			break
		}
	}
	switch {
	case h == "":
		return "", &InvalidError{Msg: "host must not be empty"}
	case len(h) > 253:
		return "", &InvalidError{Msg: "host is longer than 253 characters"}
	case strings.Contains(h, "*"):
		return "", &InvalidError{Msg: "host must name one host; a wildcard entry is *. followed by a host name, such as *.example.com"}
	case strings.ContainsAny(h, "/:@?#"):
		return "", &InvalidError{Msg: "host must be a bare host name such as docs.example.com, without a scheme, port or path"}
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return "", &InvalidError{Msg: "host must be a DNS name, not an IP address"}
	}
	if !hostPattern.MatchString(h) {
		return "", &InvalidError{Msg: "host must be a DNS name of letters, digits, '-' and '.'"}
	}
	last := h[strings.LastIndexByte(h, '.')+1:]
	switch {
	case !strings.Contains(h, "."):
		return "", &InvalidError{Msg: "host must have at least two labels, such as docs.example.com; a single-label name such as localhost cannot be trusted"}
	case numericLabel.MatchString(last):
		return "", &InvalidError{Msg: "host must be a DNS name, not a number a resolver would read as an IP address"}
	}
	return h, nil
}

// ValidateMatchThreshold checks a match threshold against the bounds the guard supports.
func ValidateMatchThreshold(n int) error {
	if n < leakguard.MinThreshold || n > leakguard.MaxThreshold {
		return &InvalidError{Msg: fmt.Sprintf("match threshold must be %d to %d characters", leakguard.MinThreshold, leakguard.MaxThreshold)}
	}
	return nil
}

// LeakGuardOf returns the org's leak-guard configuration read through q.
func LeakGuardOf(ctx context.Context, q *store.Queries, org pgtype.UUID) (LeakGuard, error) {
	out := LeakGuard{MatchThreshold: DefaultMatchThreshold, TrustedHosts: []string{}, CodeHostRepos: []sinkclass.Repo{}}
	row, err := q.GetOrgSettings(ctx, org)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return LeakGuard{}, err
	case row.LeakMatchThreshold.Valid:
		out.MatchThreshold = int(row.LeakMatchThreshold.Int32)
	}
	hosts, err := q.ListOrgFetchTrustedHosts(ctx, org)
	if err != nil {
		return LeakGuard{}, err
	}
	for _, h := range hosts {
		out.TrustedHosts = append(out.TrustedHosts, h.Host)
	}
	repos, err := q.ListOrgCodeHostRepos(ctx, org)
	if err != nil {
		return LeakGuard{}, err
	}
	out.CodeHostRepos = codeHostScopeOf(repos)
	return out, nil
}

// Service changes an org's leak-guard settings and audits each change.
type Service struct{ pool *pgxpool.Pool }

// NewService returns a Service on pool.
func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// tx runs fn in the transaction of an audited change: events go into b, and the audit module writes
// them in lock order just before commit (ADR 0100 decision 1.7).
func (s *Service) tx(ctx context.Context, fn func(q *store.Queries, b audit.Batch) error) error {
	_, err := audit.InTx(ctx, s.pool, fn)
	return err
}

// LeakGuard returns the org's effective leak-guard configuration.
func (s *Service) LeakGuard(ctx context.Context, org pgtype.UUID) (LeakGuard, error) {
	return LeakGuardOf(ctx, store.New(s.pool), org)
}

// SetMatchThreshold sets the org's match threshold; nil returns it to the platform default. A change
// is audited as org.leak_guard.threshold.set.
func (s *Service) SetMatchThreshold(ctx context.Context, org pgtype.UUID, actor audit.Actor, n *int) error {
	set := pgtype.Int4{}
	if n != nil {
		if err := ValidateMatchThreshold(*n); err != nil {
			return err
		}
		set = pgtype.Int4{Int32: int32(*n), Valid: true} //nolint:gosec // bounded by ValidateMatchThreshold
	}
	return s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		before, err := LeakGuardOf(ctx, q, org)
		if err != nil {
			return err
		}
		if err := q.SetOrgLeakMatchThreshold(ctx, store.SetOrgLeakMatchThresholdParams{OrgID: org, LeakMatchThreshold: set}); err != nil {
			return err
		}
		after := DefaultMatchThreshold
		if n != nil {
			after = *n
		}
		if before.MatchThreshold == after {
			return nil
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "org.leak_guard.threshold.set", TargetKind: "org", TargetID: org.String(),
			Before: map[string]any{"matchThreshold": before.MatchThreshold}, After: map[string]any{"matchThreshold": after},
		})
		return nil
	})
}

// AddTrustedHost puts host on the org's fetch-trusted list and returns the name it was stored under.
// A host already on the list changes nothing. Audited as org.fetch_trusted_host.add.
func (s *Service) AddTrustedHost(ctx context.Context, org pgtype.UUID, actor audit.Actor, host string) (string, error) {
	h, err := NormalizeTrustedEntry(host)
	if err != nil {
		return "", err
	}
	err = s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		// The cap is checked under a lock on the org's settings row, so parallel adds cannot each see room
		// for one more host.
		if err := q.EnsureOrgSettings(ctx, org); err != nil {
			return err
		}
		if _, err := q.LockOrgSettings(ctx, org); err != nil {
			return err
		}
		have, err := q.ListOrgFetchTrustedHosts(ctx, org)
		if err != nil {
			return err
		}
		if len(have) >= MaxTrustedHosts {
			for _, x := range have {
				if x.Host == h {
					return nil
				}
			}
			return &InvalidError{Msg: fmt.Sprintf("the fetch-trusted list holds at most %d hosts", MaxTrustedHosts)}
		}
		n, err := q.AddOrgFetchTrustedHost(ctx, store.AddOrgFetchTrustedHostParams{OrgID: org, Host: h})
		if err != nil || n == 0 {
			return err
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "org.fetch_trusted_host.add", TargetKind: "org_fetch_trusted_host", TargetID: h,
			Before: map[string]any{"trusted": false}, After: map[string]any{"host": h, "trusted": true},
		})
		return nil
	})
	return h, err
}

// RemoveTrustedHost takes host off the org's fetch-trusted list; ErrNotFound when it was not on it.
// Audited as org.fetch_trusted_host.remove.
func (s *Service) RemoveTrustedHost(ctx context.Context, org pgtype.UUID, actor audit.Actor, host string) error {
	h, err := NormalizeTrustedEntry(host)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		n, err := q.DeleteOrgFetchTrustedHost(ctx, store.DeleteOrgFetchTrustedHostParams{OrgID: org, Host: h})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "org.fetch_trusted_host.remove", TargetKind: "org_fetch_trusted_host", TargetID: h,
			Before: map[string]any{"host": h, "trusted": true}, After: map[string]any{"trusted": false},
		})
		return nil
	})
}
