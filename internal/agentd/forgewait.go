// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/gateway/egress"
	"github.com/stateracloud/koinon/internal/gateway/route"
)

// DefaultForgeServeTimeout bounds the wait for the gateway to serve a run's forge route. A new
// forge credential file takes up to the reconciler's settle time to reach the gateway pod (more than
// 74 s on kind, #1086 part C2), then a pass applies it; five minutes is several times that.
const DefaultForgeServeTimeout = 5 * time.Minute

// forgeProbeTimeout bounds each probe request.
const forgeProbeTimeout = 10 * time.Second

// forgeProbeTTL is the lifetime of the probe's own actor token: the shortest that the minter
// accepts. The token never leaves koinon-agentd.
const forgeProbeTTL = actortoken.MinTTL

// forgeWaitReasons are the refusals of the egress listener that end when the reconciler's next
// pass reaches the gateway: the credential file is still settling (credential_unavailable), the
// gateway has no route table yet (no_table) or not yet the agent's routes (no_route), or the pod is
// draining.
var forgeWaitReasons = map[string]bool{
	string(egress.ReasonNoCredential): true,
	string(route.ReasonNoTable):       true,
	string(route.ReasonNoRoute):       true,
	string(egress.ReasonDraining):     true,
}

// waitForge waits until the gateway's egress listener serves the agent's forge route, so the
// agent's first git command is not refused while the forge credential still reaches the gateway
// (#1086 part C2). The reconciler leaves a new forge credential out of the gateway state until its
// file has settled, and the route fails closed meanwhile.
//
// koinon-agentd sends the request that git sends first (info/refs of git-upload-pack) for the
// first bound repository, with its own short-lived actor token, outside the guest. A refusal in
// forgeWaitReasons, or a gateway that cannot be reached, means wait. Any other answer ends the wait:
// the gateway attached the credential and sent the request on (the forge's own answer, or
// upstream_error), or it refuses for a reason that no wait changes, which git then shows to the
// agent. The wait never opens a route; the gateway gates every git request as before. A run without
// a bound repository has no request to send and does not wait. After ForgeServeTimeout the run
// fails with a reason that names the code host.
func (r *runner) waitForge(ctx context.Context, h codeHost, repos []repository) error {
	if len(repos) == 0 {
		r.log.Info("no bound repository: the run does not wait for the gateway's forge route")
		return nil
	}
	probe := forgeProbeURL(r.p.cfg.EgressURL, h, repos[0])
	a, err := r.actor(ctx)
	if err != nil {
		return err
	}
	now := r.p.cfg.Now
	started := now()
	var (
		waiting bool
		nudged  time.Time
		poll    = r.p.cfg.Poll
	)
	for {
		tok, err := r.p.cfg.ActorTokens.Mint(a, forgeProbeTTL)
		if err != nil {
			return fmt.Errorf("mint the forge probe's actor token: %w", err)
		}
		reason, err := r.probeForge(ctx, probe, tok)
		switch {
		case err != nil && ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			reason = "unreachable"
			r.log.Warn("probing the gateway's forge route failed", "host", h.loc.Web.Authority(), "err", err)
		case !forgeWaitReasons[reason]:
			if waiting {
				r.log.Info("the gateway serves the forge route now", "host", h.loc.Web.Authority(), "waited", now().Sub(started))
			}
			return nil
		}
		if !waiting {
			waiting = true
			r.log.Info("waiting for the forge credential to reach the gateway", "host", h.loc.Web.Authority(), "refusal", reason)
		}
		if waited := now().Sub(started); waited >= r.p.cfg.ForgeServeTimeout {
			return &failure{reason: fmt.Sprintf("the Koinon gateway did not serve the forge route of the %s host %s within %s "+
				"(its egress listener answers %s). Check the code-host connection and the koinon-reconciler log for why "+
				"its credential does not reach the gateway, then run the task again",
				h.connector, h.loc.Web.Authority(), r.p.cfg.ForgeServeTimeout, reason)}
		}
		if now().Sub(nudged) >= r.p.cfg.ReconcileNudge {
			nudged = now()
			if err := r.q.NotifyReconciler(ctx, r.run.OrgID); err != nil && ctx.Err() == nil {
				r.log.Warn("notifying the reconciler failed", "err", err)
			}
		}
		if !sleep(ctx, poll) {
			return ctx.Err()
		}
		// Back off to a few seconds: a credential takes a minute or more to reach the gateway.
		poll = min(2*poll, max(r.p.cfg.Poll, 5*time.Second))
	}
}

// probeForge sends one probe with the actor token tok and returns the listener's refusal reason, or "" for an answer that
// is not a refusal of the listener. The token is never logged; the
// response body is read and dropped.
func (r *runner) probeForge(ctx context.Context, probe, tok string) (string, error) {
	pctx, cancel := context.WithTimeout(ctx, forgeProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, probe, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set(codehost.ActorTokenHeader, tok)
	req.Header.Set("Git-Protocol", "version=2")
	resp, err := r.p.cfg.EgressProbe.Do(req)
	if err != nil {
		// The URL holds the forge path but never the token.
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.Header.Get(egress.RefusalHeader), nil
}

// forgeProbeURL is the URL that git's first request of a clone of repo reaches through the egress
// prefix (codehost.EgressGitConfig rewrites https://<host>/ to it).
func forgeProbeURL(base string, h codeHost, repo repository) string {
	rest := strings.TrimPrefix(repo.CloneURL, "https://"+h.loc.Web.Authority()+"/")
	return codehost.EgressPrefix(base, h.loc) + strings.TrimSuffix(rest, ".git") + ".git/info/refs?service=git-upload-pack"
}
