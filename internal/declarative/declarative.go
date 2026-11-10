// SPDX-License-Identifier: Apache-2.0

// Package declarative imports Kubernetes Secrets that declare an org's model providers and
// connections (ADR 0056, plan item PV1), in the style of Argo CD's declarative setup
// (https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/):
//
//	apiVersion: v1
//	kind: Secret
//	metadata:
//	  name: openrouter
//	  labels:
//	    koinon.dev/secret-type: model-provider   # or: connection, mcp-server
//	    koinon.dev/org: acme                     # the org's slug
//	stringData:
//	  name: openrouter
//	  provider: openrouter
//	  url: https://openrouter.ai/api/v1
//	  apiKey: sk-or-...
//	  models: |
//	    - name: deepseek/deepseek-v4.1-flash
//	      guard: true
//	      format: openai          # optional: the model's wire format, within what the provider offers (ADR 0069)
//
// A provider of type anthropic may also set anthropicWorkspaceId (wrkspc_...), the workspace the
// gateway names for an API key not scoped to one.
//
// A provider of type anthropic or openai at its own API URL may also set costApiKey (an admin key
// that may read the provider's cost API) with costScope (the Anthropic workspace ID or "default",
// or the OpenAI project ID or API key ID). Koinon-reconciler reads the cost API once a day and
// records the ledger's difference. The key is sealed like apiKey and never returned or logged;
// leaving both out removes a stored cost key.
//
// A Secret that parses becomes a provider (with its models) or a connection with source
// "secret": read-only through the UI and admin API, winning a name collision with a UI entry
// (which is disabled, with a visible reason), and removed when the Secret is deleted. A
// Secret that does not parse is ignored: a Warning Event on the Secret and a log line say why,
// and what it declared before stays. Every apply and removal is audited with the actor
// "secret <name>".
//
// # Where it runs
//
// In koinon-api, not the reconciler. The importer writes through the same service layer as the
// UI and admin API (audit, validation, sealing with the KEK), and koinon-api already holds the
// Kubernetes client and namespace for Secrets and the KEKs. The reconciler is single-active and
// renders gateway state; it picks the imported rows up on its next pass like any other row.
// koinon-api runs several replicas: every replica imports, and the version check and the
// transactions make that idempotent.
//
// # Trust boundary
//
// Write access to Koinon's namespace is platform-admin level: anyone who can create a
// labelled Secret there can define providers and credentials for any org. Grant it as you
// would grant cluster administration.
package declarative

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/gitea"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/outward"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/restorehold"
)

// Event reasons.
const (
	ReasonInvalid = "InvalidKoinonSecret"
	ReasonApplied = "AppliedByKoinon"
)

// Importer imports the declarative Secrets of one namespace.
type Importer struct {
	Client    kubernetes.Interface
	Namespace string
	Pool      *pgxpool.Pool
	Catalog   *catalog.Service
	// MCP imports mcp-server Secrets (plan item MC1); nil ignores them with a Warning Event.
	MCP *mcpservers.Service
	// Outward applies the disabled key of a Secret (ADR 0110 decision 5); nil ignores the key.
	Outward *outward.Service
	// URLPolicy decides which hosts a declared connection's base URL may name.
	URLPolicy catalog.URLPolicy
	// Sealer seals declared connections' secret fields.
	Sealer connections.Sealer
	Log    *slog.Logger
	// Resync is the time between full passes without a change event. Default 5 minutes.
	Resync time.Duration

	// orgBackoff is the first wait before re-reading Secrets that name an org not created yet;
	// zero means defaultOrgBackoff. The wait doubles up to Resync. Set by tests only.
	orgBackoff time.Duration
	// syncFn replaces sync in tests of the Run loop.
	syncFn func(ctx context.Context) (bool, error)
	// after returns a channel that fires once d has passed; nil means time.After. Tests record d.
	after func(d time.Duration) <-chan time.Time

	mu     sync.Mutex
	warned map[string]string // secret -> version already warned about
}

// defaultOrgBackoff is the first retry delay for a Secret whose org does not exist yet.
const defaultOrgBackoff = 5 * time.Second

// Selector selects the Secrets the importer reads.
func Selector() string {
	return LabelSecretType + " in (" + TypeModelProvider + "," + TypeConnection + "," + TypeMCPServer + ")"
}

// Run imports on every change to a labelled Secret and on a timer, until ctx ends. While a
// Secret waits for its org, which another process may create at any moment, or a pass ends in
// an error (a lock, a List or a lookup that failed once), it also retries on a backoff that
// starts at 5 seconds and doubles up to the resync interval; the backoff starts over after a
// pass that neither waits nor fails and on a Secret event. Once the backoff has reached the
// resync interval no retry is requested, because the ticker is that retry: a slug that never
// resolves, such as an org that is never created, settles at one pass per resync interval.
func (i *Importer) Run(ctx context.Context) {
	resync := i.Resync
	if resync <= 0 {
		resync = 5 * time.Minute
	}
	wake := make(chan struct{}, 1)
	kick := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	factory := informers.NewSharedInformerFactoryWithOptions(i.Client, 0,
		informers.WithNamespace(i.Namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = Selector() }))
	inf := factory.Core().V1().Secrets().Informer()
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { kick() },
		UpdateFunc: func(_, _ any) { kick() },
		DeleteFunc: func(any) { kick() },
	})
	factory.Start(ctx.Done())

	syncOnce := i.sync
	if i.syncFn != nil {
		syncOnce = i.syncFn
	}
	first := i.orgBackoff
	if first <= 0 {
		first = defaultOrgBackoff
	}
	first = min(first, resync)
	backoff := first
	after := i.after
	if after == nil {
		after = time.After
	}
	var retry <-chan time.Time

	t := time.NewTicker(resync)
	defer t.Stop()
	kick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			backoff = first
			time.Sleep(200 * time.Millisecond) // coalesce a burst of events
		case <-t.C:
		case <-retry:
		}
		select {
		case <-wake:
		default:
		}
		waiting, err := syncOnce(ctx)
		if err != nil && ctx.Err() == nil {
			i.log().Error("importing declarative Secrets failed", "err", err)
		}
		retry = nil
		switch {
		case !waiting && err == nil:
			backoff = first
		case backoff < resync:
			retry = after(backoff)
			backoff = min(backoff*2, resync)
		}
		// Else the backoff has reached the resync interval, which the ticker covers.
	}
}

func (i *Importer) log() *slog.Logger {
	if i.Log == nil {
		return slog.Default()
	}
	return i.Log
}

type present struct {
	org, typ, name string
}

// Sync reads every labelled Secret and converges the database to them. Per-Secret problems
// are reported on the Secret and do not stop the others; the returned error is for failures
// that stopped the whole pass.
func (i *Importer) Sync(ctx context.Context) error {
	_, err := i.sync(ctx)
	return err
}

// sync is Sync that also reports whether a Secret was rejected only because its org does not
// exist yet, which may change without any Secret event.
func (i *Importer) sync(ctx context.Context) (waitingForOrg bool, err error) {
	// One replica imports at a time; the others wait, then find nothing changed.
	lock, err := i.Pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("lock the import: %w", err)
	}
	defer lock.Release()
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(syncLockKey)); err != nil {
		return false, fmt.Errorf("lock the import: %w", err)
	}
	defer func() {
		_, _ = lock.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(syncLockKey))
	}()

	list, err := i.Client.CoreV1().Secrets(i.Namespace).List(ctx, metav1.ListOptions{LabelSelector: Selector()})
	if err != nil {
		// Not listing means not knowing which Secrets exist: remove nothing.
		return false, fmt.Errorf("list declarative Secrets: %w", err)
	}
	secrets := list.Items
	sort.SliceStable(secrets, func(a, b int) bool {
		ta, tb := secrets[a].CreationTimestamp, secrets[b].CreationTimestamp
		if !ta.Equal(&tb) {
			return ta.Before(&tb)
		}
		return secrets[a].Name < secrets[b].Name
	})

	q := store.New(i.Pool)
	orgs := map[string]pgtype.UUID{}
	orgOf := func(slug string) (pgtype.UUID, bool, error) {
		if id, ok := orgs[slug]; ok {
			return id, true, nil
		}
		o, err := q.GetOrgBySlug(ctx, slug)
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, false, nil
		}
		if err != nil {
			return pgtype.UUID{}, false, err
		}
		orgs[slug] = o.ID
		return o.ID, true, nil
	}

	seen := map[present]bool{}
	for k := range secrets {
		sec := &secrets[k]
		typ, slug := sec.Labels[LabelSecretType], sec.Labels[LabelOrg]
		version := string(sec.UID) + "/" + sec.ResourceVersion
		org, ok, err := orgOf(slug)
		if err != nil {
			return waitingForOrg, fmt.Errorf("look up org %q: %w", slug, err)
		}
		if !ok {
			// An empty slug (no org label) never resolves, so it does not earn a retry.
			waitingForOrg = waitingForOrg || slug != ""
			i.reject(ctx, sec, version, fmt.Errorf("label %s=%q names no org", LabelOrg, slug))
			continue
		}
		seen[present{slug, typ, sec.Name}] = true
		var applyErr error
		switch typ {
		case TypeModelProvider:
			applyErr = i.applyProvider(ctx, org, sec, version)
		case TypeConnection:
			applyErr = i.applyConnection(ctx, org, sec, version)
		case TypeMCPServer:
			applyErr = i.applyMCPServer(ctx, org, sec, version)
		}
		if applyErr == nil {
			applyErr = i.syncDisabled(ctx, org, typ, sec)
		}
		if applyErr != nil {
			var ie *invalidError
			if !errors.As(applyErr, &ie) && !isUserError(applyErr) {
				return waitingForOrg, fmt.Errorf("apply Secret %s: %w", sec.Name, applyErr)
			}
			i.reject(ctx, sec, version, applyErr)
		}
	}
	return waitingForOrg, i.removeAbsent(ctx, q, seen)
}

// syncDisabled applies the Secret's disabled key to the items the Secret manages (the outward switch).
func (i *Importer) syncDisabled(ctx context.Context, org pgtype.UUID, typ string, sec *corev1.Secret) error {
	if i.Outward == nil {
		return nil
	}
	disabled, err := ParseDisabled(sec)
	if err != nil {
		return &invalidError{err}
	}
	kind := map[string]outward.Kind{TypeModelProvider: outward.ModelProvider, TypeConnection: outward.CodeHostConnection, TypeMCPServer: outward.MCPServer}[typ]
	n, err := i.Outward.SyncDeclared(ctx, org, kind, sec.Name, disabled)
	if err != nil {
		return err
	}
	if n > 0 {
		i.log().Info("applied the disabled key of a declarative Secret", "secret", sec.Name, "org", sec.Labels[LabelOrg], "kind", string(kind), "disabled", disabled, "items", n)
	}
	return nil
}

type invalidError struct{ err error }

func (e *invalidError) Error() string { return e.err.Error() }
func (e *invalidError) Unwrap() error { return e.err }

// isUserError reports whether err is about the Secret's content or the install's setup, which
// the Secret's Event can say, rather than a failure that stops the pass.
func isUserError(err error) bool {
	var inv *catalog.InvalidError
	return errors.As(err, &inv) || errors.Is(err, catalog.ErrConflict) ||
		errors.Is(err, catalog.ErrNoKeys) || errors.Is(err, connections.ErrNoKeyring)
}

// syncLockKey is the Postgres advisory lock that serialises Sync across koinon-api replicas.
const syncLockKey = 0x6b6f696e6f6e5359 // "koinonSY"

func (i *Importer) applyProvider(ctx context.Context, org pgtype.UUID, sec *corev1.Secret, version string) error {
	d, err := ParseProvider(sec, version)
	if err != nil {
		return &invalidError{err}
	}
	changed, err := i.Catalog.ApplyDeclaredProvider(ctx, org, d)
	if err != nil {
		return err
	}
	if changed {
		i.log().Info("applied a declarative provider", "secret", sec.Name, "org", sec.Labels[LabelOrg], "provider", d.Name, "models", len(d.Models))
		i.event(ctx, sec, corev1.EventTypeNormal, ReasonApplied, "Provider "+d.Name+" imported for org "+sec.Labels[LabelOrg])
	}
	// Every sync, while the Secret still carries the ignored field: the Event's count rises.
	for _, w := range d.Warnings {
		i.log().Warn("declarative provider Secret has an ignored field", "secret", sec.Name, "warning", w)
		i.event(ctx, sec, corev1.EventTypeWarning, ReasonInvalid, w)
	}
	return nil
}

func (i *Importer) applyMCPServer(ctx context.Context, org pgtype.UUID, sec *corev1.Secret, version string) error {
	d, err := ParseMCPServer(sec, version)
	if err != nil {
		return &invalidError{err}
	}
	if i.MCP == nil {
		return &invalidError{errors.New("remote MCP servers are not configured on this install")}
	}
	changed, err := i.MCP.ApplyDeclared(ctx, org, d)
	if errors.Is(err, restorehold.ErrHeld) {
		// The change would dial the server; the rest of the pass goes on (ADR 0107).
		i.log().Info("held: MCP server "+d.Name+" not applied until release", "secret", sec.Name, "org", sec.Labels[LabelOrg])
		return nil
	}
	if err != nil {
		return err
	}
	if changed {
		i.log().Info("applied a declarative MCP server", "secret", sec.Name, "org", sec.Labels[LabelOrg], "server", d.Name)
		i.event(ctx, sec, corev1.EventTypeNormal, ReasonApplied, "MCP server "+d.Name+" imported for org "+sec.Labels[LabelOrg])
	}
	return nil
}

func (i *Importer) applyConnection(ctx context.Context, org pgtype.UUID, sec *corev1.Secret, version string) error {
	d, err := ParseConnection(sec, version)
	if err != nil {
		return &invalidError{err}
	}
	if i.Sealer == nil {
		return connections.ErrNoKeyring
	}
	if d.Site.BaseURL != "" {
		if err := i.URLPolicy.Check(ctx, d.Site.BaseURL); err != nil {
			return &invalidError{fmt.Errorf("%s: %w", KeyBaseURL, err)}
		}
	}
	q := store.New(i.Pool)
	all, err := q.ListConnections(ctx, org)
	if err != nil {
		return err
	}
	var cur *store.Connection
	for k, c := range all {
		if c.Source == store.ConfigSourceSecret && c.ManagedBySecret.String == sec.Name {
			cur = &all[k]
		}
	}
	// A lost secret (keys forget) is written again from the Secret, which still holds it.
	if cur != nil && cur.DeclaredVersion.String == version && !cur.RemovedAt.Valid && !cur.CredentialLostAt.Valid {
		return nil
	}
	var prevConfig []byte
	if cur != nil {
		prevConfig = cur.Config
	}
	config, err := connectionConfig(d, prevConfig, time.Now())
	if err != nil {
		return err
	}
	actor := audit.Secret(sec.Name)
	_, err = audit.InTx(ctx, i.Pool, func(tq *store.Queries, b audit.Batch) error {
		after := map[string]any{
			"connector": d.Connector, "displayName": d.DisplayName, "appId": d.AppID,
			"installationId": d.InstallationID, "baseUrl": d.Site.BaseURL, "ciDirs": d.Site.CIDirs, "keyFingerprint": d.Fingerprint, "source": "secret",
		}
		if d.Connector == gitea.HostID {
			after = map[string]any{
				"connector": d.Connector, "displayName": d.DisplayName, "baseUrl": d.Site.BaseURL, "ciDirs": d.Site.CIDirs,
				"fingerprints": d.Fingerprint, "source": "secret",
			}
		}
		if cur == nil {
			id, err := newConnID()
			if err != nil {
				return err
			}
			env, err := connections.SealFields(i.Sealer, org, id, d.SecretFields())
			if err != nil {
				return err
			}
			if _, err := tq.CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
				OrgID: org, ID: id, Connector: d.Connector, DisplayName: d.DisplayName, Config: config,
				SecretEnvelope: env, SecretFingerprint: d.Fingerprint, Source: store.ConfigSourceSecret,
				ManagedBySecret: pgtype.Text{String: sec.Name, Valid: true}, DeclaredVersion: pgtype.Text{String: version, Valid: true},
			}); err != nil {
				return err
			}
			b.Org(audit.Event{Org: org, Actor: actor, Action: "connection.declare", TargetKind: "connection", TargetID: id.String(), After: after})
			return nil
		}
		if cur.Connector != d.Connector {
			return &invalidError{fmt.Errorf("%s cannot change from %s to %s; delete the Secret and declare a new one", KeyConnector, cur.Connector, d.Connector)}
		}
		env, err := connections.SealFields(i.Sealer, org, cur.ID, d.SecretFields())
		if err != nil {
			return err
		}
		// Row lock first (FOR UPDATE, which conflicts with the FOR KEY SHARE of a scope replacement),
		// then the scope: the order SetConnectionSecret uses. A plain UPDATE would take only FOR NO
		// KEY UPDATE, and a concurrent ReplaceCodeHostRepos would read the old fingerprint (#244).
		locked, err := tq.GetConnectionForUpdate(ctx, store.GetConnectionForUpdateParams{OrgID: org, ID: cur.ID})
		if err != nil {
			return err
		}
		updated, err := tq.UpdateDeclaredConnection(ctx, store.UpdateDeclaredConnectionParams{
			OrgID: org, ID: cur.ID, DisplayName: d.DisplayName, Config: config, SecretEnvelope: env,
			SecretFingerprint: d.Fingerprint, DeclaredVersion: pgtype.Text{String: version, Valid: true},
		})
		if err != nil {
			return err
		}
		// A new secret or site reaches other repositories than the old one read (#244).
		if locked.SecretFingerprint != d.Fingerprint || !bytes.Equal(locked.Config, updated.Config) {
			if err := orgsettings.ClearCodeHostScope(ctx, tq, org, cur.ID); err != nil {
				return err
			}
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "connection.declare", TargetKind: "connection", TargetID: cur.ID.String(),
			Before: map[string]any{"displayName": cur.DisplayName, "keyFingerprint": cur.SecretFingerprint}, After: after,
		})
		// A new baseUrl can leave MCP servers linked to a host that is no longer the connection's (#117).
		return mcpservers.UnlinkMismatchedServers(ctx, tq, b, actor, updated)
	})
	if err != nil {
		return err
	}
	i.log().Info("applied a declarative connection", "secret", sec.Name, "org", sec.Labels[LabelOrg], "key_fingerprint", d.Fingerprint)
	i.event(ctx, sec, corev1.EventTypeNormal, ReasonApplied, "Connection "+d.DisplayName+" imported for org "+sec.Labels[LabelOrg])
	return nil
}

// removeAbsent deletes what a Secret declared when the Secret is gone (or no longer carries
// the label, org or type that declared it).
func (i *Importer) removeAbsent(ctx context.Context, q *store.Queries, seen map[present]bool) error {
	orgIDs, err := q.ListOrgIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range orgIDs {
		slug, err := orgSlug(ctx, i.Pool, id)
		if err != nil {
			return err
		}
		names, err := i.Catalog.DeclaredProviderSecrets(ctx, id)
		if err != nil {
			return err
		}
		for _, name := range names {
			if !seen[present{slug, TypeModelProvider, name}] {
				if err := i.Catalog.RemoveDeclared(ctx, id, name); err != nil {
					return err
				}
				i.log().Info("removed the providers of a deleted Secret", "secret", name, "org", slug)
			}
		}
		if i.MCP != nil {
			names, err := i.MCP.DeclaredSecrets(ctx, id)
			if err != nil {
				return err
			}
			for _, name := range names {
				if !seen[present{slug, TypeMCPServer, name}] {
					if err := i.MCP.RemoveDeclared(ctx, id, name); err != nil {
						return err
					}
					i.log().Info("disabled the MCP servers of a deleted Secret", "secret", name, "org", slug)
				}
			}
		}
		conns, err := q.ListConnections(ctx, id)
		if err != nil {
			return err
		}
		for _, c := range conns {
			if c.Source != store.ConfigSourceSecret || c.RemovedAt.Valid || seen[present{slug, TypeConnection, c.ManagedBySecret.String}] {
				continue
			}
			// Marked removed, not deleted: agents are bound to it and the audit log names it.
			_, err := audit.InTx(ctx, i.Pool, func(tq *store.Queries, b audit.Batch) error {
				if err := tq.MarkConnectionRemoved(ctx, store.MarkConnectionRemovedParams{OrgID: id, ID: c.ID}); err != nil {
					return err
				}
				b.Org(audit.Event{
					Org: id, Actor: audit.Secret(c.ManagedBySecret.String), Action: "connection.undeclare", TargetKind: "connection",
					TargetID: c.ID.String(), Before: map[string]any{"displayName": c.DisplayName, "keyFingerprint": c.SecretFingerprint},
					After: map[string]any{"removed": true},
				})
				return nil
			})
			if err != nil {
				return err
			}
			i.log().Warn("marked the connection of a deleted Secret removed; agents bound to it report why, and restoring the Secret restores it", "secret", c.ManagedBySecret.String, "org", slug)
		}
	}
	return nil
}

// reject reports an invalid Secret once per version.
func (i *Importer) reject(ctx context.Context, sec *corev1.Secret, version string, err error) {
	i.mu.Lock()
	if i.warned == nil {
		i.warned = map[string]string{}
	}
	already := i.warned[sec.Name] == version
	i.warned[sec.Name] = version
	i.mu.Unlock()
	if already {
		return
	}
	// The messages never quote a value, so they are safe in a log and an Event.
	i.log().Warn("ignoring an invalid declarative Secret", "secret", sec.Name, "type", sec.Labels[LabelSecretType], "org", sec.Labels[LabelOrg], "err", err)
	i.event(ctx, sec, corev1.EventTypeWarning, ReasonInvalid, "Ignored: "+err.Error())
}

// event records a Kubernetes Event on the Secret. Failing to record one is only logged.
func (i *Importer) event(ctx context.Context, sec *corev1.Secret, typ, reason, msg string) {
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	now := metav1.NewTime(time.Now())
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s.%x", sec.Name, time.Now().UnixNano()), Namespace: i.Namespace},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Secret", Namespace: i.Namespace, Name: sec.Name, UID: sec.UID, APIVersion: "v1",
		},
		Type: typ, Reason: reason, Message: msg, FirstTimestamp: now, LastTimestamp: now, Count: 1,
		Source: corev1.EventSource{Component: project.Name + "-api"},
	}
	if _, err := i.Client.CoreV1().Events(i.Namespace).Create(ctx, ev, metav1.CreateOptions{}); err != nil {
		i.log().Warn("could not record a Kubernetes Event", "secret", sec.Name, "err", err)
	}
}
