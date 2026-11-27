// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
)

// Git through the gateway's egress prefix (ADR 0097, ADR 0120 section 1, #1086 part C2).
//
// When the pool has a minter (Config.ActorTokens), a run whose agent has a code-host connection
// sends git to <EgressURL>/egress/<host>/ instead of the host. The gateway checks the actor token,
// matches the agent's forge routes and attaches the credential, so the guest holds no credential.
//
// koinon-agentd mints the token here, outside the guest, and writes it into the sandbox through the
// driver's exec, which both drivers serve the same way. The token names the org (its slug, as the
// gateway routes do), the agent (its ID, the gateway agent name) and the run. It lives
// actorTokenTTL and is written again every Config.ActorTokenRefresh, so it never ends while the run
// is live. It is written outside the home (codehost.SandboxEgressToken), never into it, and it is
// removed when the run ends. It is never logged. The agent user can read the file while the
// run is live: an interim of ADR 0120 (amendment of 2026-11-26) whose exit is issue #1386.

// actorTokenTTL is the lifetime of each token: well inside 300 to 3600 seconds (ADR 0120 1.2), and
// three times the default refresh, so one failed refresh does not stop git.
const actorTokenTTL = 15 * time.Minute

// DefaultActorTokenRefresh is how often a live run writes a new actor token.
const DefaultActorTokenRefresh = 5 * time.Minute

// actorTokenWriteTimeout bounds each write and the final remove of the token file.
const actorTokenWriteTimeout = 30 * time.Second

// Agent Substrate (Config.ActorIdentityOutside, issue #1386, ADR 0120 amendment of 2026-11-26) differs:
// atenet-egress, outside the guest, replaces the public placeholder in the actor token header with the
// actor's own token. koinon-agentd then mints no token and writes no token file, and the guest's git
// configuration sends only actortoken.Placeholder (codehost.EgressPlaceholderGitConfig).

// egressGit reports whether git of a run on code host h goes through the gateway.
func (r *runner) egressGit(h codeHost) bool {
	return (r.p.cfg.ActorTokens != nil || r.tokenOutside()) && h.connector != ""
}

// tokenOutside reports whether git goes through the gateway with the actor identity added outside the
// guest: no token file and no refresh.
func (r *runner) tokenOutside() bool {
	return r.p.cfg.ActorIdentityOutside && r.p.cfg.EgressURL != ""
}

// egressGitConfig is what the guest's git configuration adds for the gateway, and before it, on
// OpenShell, the token file it includes.
func (r *runner) egressGitConfig(ctx context.Context, workspace, sandbox string, loc connectors.Location) ([]byte, error) {
	if r.tokenOutside() {
		return codehost.EgressPlaceholderGitConfig(r.p.cfg.EgressURL, loc), nil
	}
	// The token first: the config's include of a file that is not there yet is ignored by git, but a
	// rewrite to the gateway without the token would be refused.
	if err := r.writeActorToken(ctx, workspace, sandbox); err != nil {
		return nil, err
	}
	return codehost.EgressGitConfig(r.p.cfg.EgressURL, loc), nil
}

// actor is who the run's tokens speak for.
func (r *runner) actor(ctx context.Context) (actortoken.Actor, error) {
	if r.orgSlug == "" {
		slug, err := r.q.GetOrgSlug(ctx, r.run.OrgID)
		if err != nil {
			return actortoken.Actor{}, fmt.Errorf("read the org slug: %w", err)
		}
		r.orgSlug = slug
	}
	return actortoken.Actor{Org: r.orgSlug, Agent: r.run.AgentID.String(), Run: r.run.ID.String()}, nil
}

// writeActorToken mints a token and replaces the token file in the sandbox in one rename. The error
// names the step, never the token.
func (r *runner) writeActorToken(ctx context.Context, workspace, sandbox string) error {
	a, err := r.actor(ctx)
	if err != nil {
		return err
	}
	tok, err := r.p.cfg.ActorTokens.Mint(a, actorTokenTTL)
	if err != nil {
		return fmt.Errorf("mint the actor token: %w", err)
	}
	cfg, ok := codehost.EgressTokenGitConfig(r.p.cfg.EgressURL, tok)
	if !ok {
		return fmt.Errorf("mint the actor token: the minter returned a token that is not a compact JWS")
	}
	_, code, stderr, err := r.sandboxExec(ctx, workspace, sandbox,
		codehost.WriteEgressTokenCommand(), cfg)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("write the actor token to %s: exit %d: %s", codehost.SandboxEgressToken, code, strings.TrimSpace(stderr))
	}
	return nil
}

// keepActorToken writes a new token every refresh interval until ctx ends or the run is fenced,
// then removes the file. A failed refresh is logged and tried again at the next tick; the last token
// stays valid for actorTokenTTL.
func (r *runner) keepActorToken(ctx context.Context, workspace, sandbox string) (stop func()) {
	tctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		t := time.NewTicker(r.p.cfg.ActorTokenRefresh)
		defer t.Stop()
		for {
			select {
			case <-tctx.Done():
				return
			case <-r.own.Fenced():
				return
			case <-t.C:
			}
			// The write is not cancelled with tctx: stop waits for it, so a write in flight can never
			// land after the remove below.
			wctx, done := context.WithTimeout(context.WithoutCancel(tctx), actorTokenWriteTimeout)
			if err := r.writeActorToken(wctx, workspace, sandbox); err != nil {
				r.log.Warn("refresh the actor token", "err", err)
			}
			done()
		}
	})
	return func() {
		cancel()
		wg.Wait()
		rctx, done := context.WithTimeout(context.WithoutCancel(ctx), actorTokenWriteTimeout)
		defer done()
		if _, code, _, err := r.sandboxExec(rctx, workspace, sandbox, codehost.RemoveEgressTokenCommand(), nil); err != nil || code != 0 {
			r.log.Warn("remove the actor token from the sandbox", "err", err, "exit", code)
		}
	}
}

// checkEgress checks the egress settings: ActorTokens and EgressURL go together, and the URL is a
// plain http or https base that holds nothing a guest must not see. With ActorIdentityOutside there
// are no ActorTokens, and the URL must be http.
func (c *Config) checkEgress() error {
	if c.ActorIdentityOutside {
		if c.ActorTokens != nil {
			return errors.New("agentd: ActorTokens and ActorIdentityOutside exclude each other: the egress layer adds the actor identity outside the guest")
		}
		if c.EgressURL == "" {
			return nil
		}
		if err := CheckEgressURL(c.EgressURL, true); err != nil {
			return err
		}
		c.EgressURL = strings.TrimRight(c.EgressURL, "/")
		return nil
	}
	if c.ActorTokens == nil && c.EgressURL == "" {
		return nil
	}
	if c.ActorTokens == nil || c.EgressURL == "" {
		return errors.New("agentd: ActorTokens and EgressURL go together")
	}
	if err := CheckEgressURL(c.EgressURL, false); err != nil {
		return err
	}
	c.EgressURL = strings.TrimRight(c.EgressURL, "/")
	setDefault(&c.ActorTokenRefresh, DefaultActorTokenRefresh)
	if c.ActorTokenRefresh*3 > actorTokenTTL {
		return fmt.Errorf("agentd: with an EgressURL, ActorTokenRefresh must be at most %s", actorTokenTTL/3)
	}
	return nil
}

// CheckEgressURL checks that s is a plain http or https base URL that holds nothing a guest must not
// see: it is written into the sandbox's git configuration. koinon-agentd checks its flag with it at
// start, before it reads any key. With identityOutside (Agent Substrate) it must be http: the driver puts
// the actor JWT on one rule only, the clear-text HttpRule of the egress route, and an https URL does not
// match it (ADR 0120 amendment for #1386, item 3).
func CheckEgressURL(s string, identityOutside bool) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("agentd: EgressURL must be an http or https URL, got %q", s)
	}
	if identityOutside && u.Scheme != "http" {
		return fmt.Errorf("agentd: EgressURL must be an http URL on Agent Substrate, got %q: the egress policy adds the actor token only on the clear-text egress rule", s)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" || u.Opaque != "" || u.ForceQuery {
		return errors.New("agentd: EgressURL must not carry user info, a path, a query or a fragment")
	}
	if strings.ContainsAny(s, "\"\\\n\r\t ") {
		return errors.New("agentd: EgressURL must not hold quotes, backslashes or white space")
	}
	return nil
}
