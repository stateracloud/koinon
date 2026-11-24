// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/forgecredfile"
	"github.com/stateracloud/koinon/internal/gateway/route"
	"github.com/stateracloud/koinon/internal/reconciler/gatewayplan"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// RecipientKeys gives the gateway's current recipient public key (gatewaykeys.PublicKeys, the same
// source koinon-api seals MCP credentials to). It is mcpservers.RecipientSource.
type RecipientKeys interface {
	Current() (secretstore.RecipientPublicKey, error)
}

// ForgeUser is the user name of the basic scheme the gateway sends a forge token with. GitHub takes an
// installation token this way for git over HTTPS and its API; Gitea takes any user name.
const ForgeUser = "x-access-token"

// forgeSeal is the sealed forge token of one agent, kept so a token that did not change keeps its file
// (the name holds a hash of the envelope, and every seal is new randomness).
type forgeSeal struct {
	scope string
	rev   string
	keyID string
	file  string
	env   []byte
}

// tokenRevision names a code-host token by its expiry or, for a token that does not expire, by its
// revision. It never names the value.
func tokenRevision(tok codehost.Token) string {
	if tok.Revision != "" {
		return "github-token-" + tok.Revision
	}
	return "github-token-expires-" + tok.ExpiresAt.UTC().Format(time.RFC3339)
}

// forgeCredential seals the agent's code-host token to the gateway key (#1086 part C). The gateway
// attaches it on the agent's forge routes only; the guest never holds it. It returns nil, with no
// error, when the routes name no credential or the reconciler has no gateway key source. On an error
// the agent gets no credential: its forge routes fail closed (503 credential_unavailable).
func (r *Reconciler) forgeCredential(ctx context.Context, w *work, read GitHubRead, routes []route.Route) (*gatewayplan.ForgeCredential, []byte, error) {
	if r.cfg.ForgeRecipient == nil {
		return nil, nil, nil
	}
	name := CodeHostCredentialName(agentRef(w.org, w.agent.ID))
	var hosts []string
	for _, rt := range routes {
		if rt.Credential == name {
			hosts = append(hosts, net.JoinHostPort(rt.Host, strconv.Itoa(rt.Port)))
		}
	}
	if len(hosts) == 0 {
		return nil, nil, nil
	}
	if r.cfg.GitHub == nil {
		return nil, nil, errors.New("no code-host token minter is configured")
	}
	var connID pgtype.UUID
	if err := connID.Scan(read.ConnectionID); err != nil || !connID.Valid {
		return nil, nil, fmt.Errorf("bad connection ID %q", read.ConnectionID)
	}
	conn, err := r.q.GetConnection(ctx, store.GetConnectionParams{OrgID: w.org.ID, ID: connID})
	if err != nil {
		return nil, nil, fmt.Errorf("read connection %s: %w", read.ConnectionID, err)
	}
	tok, err := r.cfg.GitHub.ReadToken(ctx, conn, codehost.Scope{Repositories: read.Repositories, Permissions: read.Permissions})
	if err != nil {
		return nil, nil, fmt.Errorf("mint the code-host token: %w", err)
	}
	pub, err := r.cfg.ForgeRecipient.Current()
	if err != nil {
		return nil, nil, fmt.Errorf("no gateway key to seal to: %w", err)
	}
	k := agentKey{w.org.ID, w.agent.ID}
	rev := tokenRevision(tok)
	scope := scopeOf(read, hosts)
	r.mu.Lock()
	s, ok := r.forgeSeals[k]
	r.mu.Unlock()
	if !ok || s.scope != scope || s.rev != rev || s.keyID != pub.ID() {
		value := []byte(tok.Value())
		env, err := secretstore.SealV2(pub, forgecredfile.Binding(w.org.ID.String(), w.agent.ID.String()), value)
		clear(value)
		if err != nil {
			return nil, nil, fmt.Errorf("seal the code-host token: %w", err)
		}
		b, err := json.Marshal(env)
		if err != nil {
			return nil, nil, fmt.Errorf("seal the code-host token: %w", err)
		}
		s = forgeSeal{scope: scope, rev: rev, keyID: pub.ID(), file: forgecredfile.SealedName(w.org.ID, w.agent.ID, scope, b), env: b}
		r.mu.Lock()
		r.forgeSeals[k] = s
		r.mu.Unlock()
	}
	return &gatewayplan.ForgeCredential{Name: name, File: s.file, Scheme: route.SchemeBasic, User: ForgeUser}, s.env, nil
}

// scopeOf names the connection and the scope a token is minted for, and the forge hosts it is sent to.
// A changed grant or host never reuses the envelope of a token minted for the old one, and the file name
// carries a hash of it, so the planner never serves the old file in its place (forgecredfile.Family).
func scopeOf(read GitHubRead, hosts []string) string {
	repos := slices.Clone(read.Repositories)
	slices.Sort(repos)
	perms := make([]string, 0, len(read.Permissions))
	for k, v := range read.Permissions {
		perms = append(perms, k+"="+v)
	}
	slices.Sort(perms)
	hosts = slices.Clone(hosts)
	slices.Sort(hosts)
	hosts = slices.Compact(hosts)
	return read.ConnectionID + "\x00" + strings.Join(repos, ",") + "\x00" + strings.Join(perms, ",") + "\x00" + strings.Join(hosts, ",")
}

// pruneForgeSeals drops the sealed tokens of agents that are not in the pass.
func (r *Reconciler) pruneForgeSeals(live map[agentKey]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.forgeSeals {
		if !live[k] {
			delete(r.forgeSeals, k)
		}
	}
}
