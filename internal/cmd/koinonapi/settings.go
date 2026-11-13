// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/url"

	"github.com/spf13/pflag"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/project"
)

// actionSettingsObserved is the install audit event for the Helm-set platform settings (ADR 0100
// decision 3).
const actionSettingsObserved = "platform.settings.observed"

// settingKind says how the value of a recorded flag is written down.
type settingKind int

const (
	// kindValue is recorded as the flag prints it.
	kindValue settingKind = iota
	// kindURL is recorded as scheme://host[:port] only: user info, path, query and fragment could carry
	// a credential.
	kindURL
	// kindURLWithPath is kindURL plus the path, for a URL whose path identifies what it names (the OIDC
	// issuer). User info, query and fragment are still dropped.
	kindURLWithPath
	// kindRef is a flag that names a Secret, a file or a directory. Only the name (the path) is
	// recorded: the thing it names is never opened, read or hashed.
	kindRef
)

// platformSetting is one recorded flag of "koinon-api serve".
type platformSetting struct {
	Flag string
	// Key is the key in the event's summary. It is the flag's name unless that name looks like a secret
	// value to the audit scrubber (audit.ErrSecretInSummary): a flag that names a file is not a secret.
	Key  string
	Kind settingKind
}

func setting(flag string, kind settingKind) platformSetting {
	return platformSetting{Flag: flag, Key: flag, Kind: kind}
}

// recordedSettings is the table of the settings that Helm sets through koinon-api's flags and that
// platform.settings.observed records (ADR 0100 decision 3, #331). A flag of "koinon-api serve" is on
// this table or on unrecordedSettings; TestFlagCoverage fails for one that is on neither, so a new flag
// forces the decision.
var recordedSettings = []platformSetting{
	// auth and sessions
	setting("bootstrap-login", kindValue),
	setting("bootstrap-admin-username", kindValue),
	{Flag: "bootstrap-password-file", Key: "bootstrap-admin-pw-file", Kind: kindRef},
	setting("default-org-identity-mode", kindValue),
	setting("default-org-group-mappings", kindValue),
	setting("oidc-issuer-url", kindURLWithPath),
	setting("oidc-client-id", kindValue),
	setting("oidc-redirect-url", kindURL),
	setting("oidc-groups-claim", kindValue),
	setting("oidc-scopes", kindValue),
	setting("oidc-platform-admin-groups", kindValue),
	setting("session-idle-timeout", kindValue),
	setting("session-max-lifetime", kindValue),
	setting("public-url", kindURL),
	setting("trusted-origins", kindValue),
	setting("trusted-proxies", kindValue),
	setting("trusted-proxy-header", kindValue),

	// network and gateway
	setting("egress-allowed-hosts", kindValue),
	setting("provider-allowed-hosts", kindValue),
	setting("guard-addr", kindValue),
	setting("leak-guard-byte-cap", kindValue),
	setting("gateway-url", kindURL),
	setting("gateway-mcp-url", kindURL),
	setting("github-api-url", kindURL),

	// keys and secrets
	{Flag: "kek-secret", Key: "kek-object", Kind: kindRef},
	setting("kek-dir", kindRef),
	setting("kek-create", kindValue),
	setting("kek-backend", kindValue),
	setting("openbao-address", kindURL),
	setting("openbao-ca-file", kindRef),
	setting("openbao-namespace", kindValue),
	setting("openbao-transit-mount", kindValue),
	setting("openbao-key", kindValue),
	setting("openbao-auth-mount", kindValue),
	setting("openbao-role", kindValue),
	{Flag: "openbao-token-file", Key: "openbao-auth-file", Kind: kindRef},
	setting("openbao-timeout", kindValue),
	setting("awskms-region", kindValue),
	setting("awskms-key-id", kindValue),
	setting("awskms-endpoint", kindURL),
	setting("awskms-sts-endpoint", kindURL),
	setting("awskms-ca-file", kindRef),
	setting("awskms-timeout", kindValue),
	{Flag: "saved-work-secret", Key: "saved-work-object", Kind: kindRef},
	setting("saved-work-addr", kindValue),

	// telemetry and scheduling
	setting("otlp-addr", kindValue),
	{Flag: "otlp-token-dir", Key: "otlp-auth-dir", Kind: kindRef},
	{Flag: "otlp-traces-token-dir", Key: "otlp-traces-auth-dir", Kind: kindRef},
	setting("durable", kindValue),
	setting("restore-hold", kindValue),
	setting("restore-accept-importer-deletions", kindValue),
	setting("run-cap-sweep-interval", kindValue),
	setting("audit-anchor-interval", kindValue),

	// the chart (the image's version is added by platformSettings)
	setting("chart-version", kindValue),
	setting("release", kindValue),
}

// imageVersionKey is the summary key of the build's version, which is not a flag.
const imageVersionKey = "image-version"

// unrecordedSettings are the flags of "koinon-api serve" that the table leaves out, each with the reason.
var unrecordedSettings = map[string]string{ //nolint:gosec // flag names and reasons, not credentials
	"database-url":                      "carries the database credentials, and the migrate Job and every replica set it",
	"oidc-client-secret-file":           "names the file holding the OIDC client secret; the chart mounts it at a fixed path",
	"prices-refresh":                    "price refresh settings are price.refresh.settings_changed (ADR 0098 decision 5, #316)",
	"prices-refresh-interval":           "price refresh settings are price.refresh.settings_changed (ADR 0098 decision 5, #316)",
	"prices-refresh-max-change-percent": "price refresh settings are price.refresh.settings_changed (ADR 0098 decision 5, #316)",
	"prices-refresh-max-auto-per-day":   "price refresh settings are price.refresh.settings_changed (ADR 0098 decision 5, #316)",
	"install-mark":                      "a random secret in a Secret (ADR 0106, ADR 0110); only its hash is recorded, in install_binding",
	"addr":                              "the pod's own listen port; the chart's Service follows it",
	"tools-addr":                        "the pod's own listen port; the chart's Service follows it",
	"metrics-addr":                      "the pod's own listen port; the chart's Service follows it",
	"migrate":                           "a development flag; the chart runs the migrate Job",
	"executor-id":                       "the pod name, different in every replica",
	"wait":                              "a start-up retry budget, not a platform setting",
	"watch-poll-interval":               "a polling backstop, not a platform setting",
	"secrets-namespace":                 "the pod's own namespace ($POD_NAMESPACE)",
	"kubeconfig":                        "a development flag; the chart uses the in-cluster config",
	"trace-queue-bytes":                 "memory tuning of the trace router, derived from the container's limit",
	"trace-intake-bytes":                "memory tuning of the trace router, derived from the container's limit",
	"trace-held-bytes":                  "memory tuning of the trace router, derived from the container's limit",
	"trace-request-bytes":               "memory tuning of the trace router, derived from the container's limit",
}

// platformSettings reads the recorded settings from the flags of "koinon-api serve" (after the flag,
// KOINON_<FLAG> and default have been resolved), keyed by Key. A flag is only read, and a Secret, file
// or directory it names is never opened. The build's version is added as image-version.
func platformSettings(fs *pflag.FlagSet) (map[string]string, error) {
	out := make(map[string]string, len(recordedSettings)+1)
	for _, s := range recordedSettings {
		f := fs.Lookup(s.Flag)
		if f == nil {
			return nil, fmt.Errorf("platform settings: koinon-api has no flag --%s", s.Flag)
		}
		out[s.Key] = s.Kind.render(f.Value.String())
	}
	out[imageVersionKey] = project.Version
	return out, nil
}

// render writes v as the event records it.
func (k settingKind) render(v string) string {
	if (k != kindURL && k != kindURLWithPath) || v == "" {
		return v
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		// Unparsable, or no URL at all: the value may hold anything, so record that it is set.
		return "(set, not a URL)"
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	if k == kindURL {
		u.Path, u.RawPath, u.Opaque = "", "", ""
	}
	return u.String()
}

// diffSettings returns the keys whose value differs between the last recorded settings and cur, as the
// event's before (only keys that had a value) and after (only keys that have one). Both are nil when
// nothing differs.
func diffSettings(prev, cur map[string]string) (before, after map[string]string) {
	for k, c := range cur {
		if p, ok := prev[k]; ok && p == c {
			continue
		}
		if after == nil {
			after = map[string]string{}
		}
		after[k] = c
		if p, ok := prev[k]; ok {
			if before == nil {
				before = map[string]string{}
			}
			before[k] = p
		}
	}
	for k, p := range prev {
		if _, ok := cur[k]; ok {
			continue
		}
		// A setting this build no longer records: it left the table.
		if before == nil {
			before = map[string]string{}
		}
		before[k] = p
	}
	return before, after
}

// foldSettings returns the settings recorded by the events, oldest first: an event carries only the
// keys that changed, so the last recorded state is every event applied in turn. nil when none was
// recorded.
func foldSettings(events []store.InstallAuditEvent) (map[string]string, error) {
	if len(events) == 0 {
		return nil, nil
	}
	state := map[string]string{}
	for _, e := range events {
		var s struct {
			Before map[string]string `json:"before"`
			After  map[string]string `json:"after"`
		}
		if err := json.Unmarshal(e.Summary, &s); err != nil {
			return nil, fmt.Errorf("read the recorded platform settings (event %d): %w", e.ChainSeq, err)
		}
		for k := range s.Before {
			delete(state, k)
		}
		maps.Copy(state, s.After)
	}
	return state, nil
}

// observeSettings is the start-up record of the Helm-set settings (ADR 0100 decision 3). In one
// transaction, under an advisory lock so that replicas starting together write once, it compares cur
// with the settings recorded so far and writes one platform.settings.observed event with only the
// changed keys in before and after. It returns whether it wrote. An unchanged restart writes nothing.
// A write the database refuses for lack of privilege returns audit.ErrInstallWriteDenied.
func observeSettings(ctx context.Context, run audit.Runner, cur map[string]string) (bool, error) {
	wrote := false
	_, err := run.InTx(ctx, func(q *store.Queries, b audit.Batch) error {
		wrote = false
		if err := q.LockPlatformSettings(ctx); err != nil {
			return fmt.Errorf("lock the platform settings record: %w", err)
		}
		events, err := q.ListInstallSettingsEvents(ctx)
		if err != nil {
			return fmt.Errorf("read the recorded platform settings: %w", err)
		}
		prev, err := foldSettings(events)
		if err != nil {
			return err
		}
		before, after := diffSettings(prev, cur)
		if before == nil && after == nil {
			return nil
		}
		ev := audit.InstallEvent{
			Actor: keyscli.ActorStartup, Action: actionSettingsObserved, TargetKind: "platform", TargetID: "settings",
			After: after,
		}
		if before != nil {
			ev.Before = before
		}
		b.Install(ev)
		wrote = true
		return nil
	})
	return wrote, err
}

// startObservingSettings records the settings once (for at most installWriteFirstAttempt), and again in the background every
// installWriteRetry while the chain refuses the write (the CloudNativePG upgrade window, ADR 0100
// decision 1.1a): the start does not wait for it. Any other failure is retried too, as for the key-set check.
func startObservingSettings(ctx context.Context, stop func(), run audit.Runner, cur map[string]string) {
	write := func(ctx context.Context) error {
		wrote, err := observeSettings(ctx, run, cur)
		if err == nil && wrote {
			slog.Info("the Helm-set platform settings differ from the last recorded ones; recorded platform.settings.observed")
		}
		return err
	}
	_ = startInstallWrite(ctx, stop, installRetry{Goroutine: "platform-settings-retry", What: "platform settings record", AnyError: true},
		installWriteRetry, installWriteFirstAttempt, write)
}
