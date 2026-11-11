// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
)

// The git setup of a connection's host (ADR 0062). The flavor images configure git for
// github.com; koinon-agentd renders the same for the host of the agent's code-host connection
// into the home before every harness start: the credential helper scoped to that host and, with
// a CA bundle, http.sslCAInfo. /etc/gitconfig includes the file. The agent can write the home,
// but nothing in these files is secret, and the credential only ever leaves the sandbox through
// OpenShell's endpoints for the connection's host.

// gitScriptMarker names the scripts below, for logs and for tests that fake the sandbox.
const (
	gitConfigMarker = "koinon-gitconfig"
	gitCAMarker     = "koinon-gitca"
)

// writeGitScript replaces $2 with stdin in one rename, in the private directory $1.
const writeGitScript = `# ` + gitConfigMarker + `
umask 077 && mkdir -p "$1" && t=$(mktemp "$1/.git.XXXXXX") && cat > "$t" && rm -rf -- "$2" && mv -f -- "$t" "$2"`

// writeCAScript writes $2 as the sandbox's system roots followed by stdin. http.sslCAInfo
// replaces git's default trust store, so the roots are taken from SSL_CERT_FILE (where OpenShell
// may deliver its interception CA) or else the image's bundle; with neither present the script
// fails rather than leave a file holding only the connection's bundle.
const writeCAScript = `# ` + gitCAMarker + `
src="${SSL_CERT_FILE:-/etc/ssl/certs/ca-certificates.crt}"
[ -s "$src" ] || { echo "no system CA roots at $src" >&2; exit 1; }
umask 077 && mkdir -p "$1" && t=$(mktemp "$1/.ca.XXXXXX") && { cat -- "$src"; cat; } > "$t" && rm -rf -- "$2" && mv -f -- "$t" "$2"`

const removeCAScript = `# ` + gitCAMarker + `
rm -rf -- "$2"`

// codeHost is the host of the run's code-host connection.
type codeHost struct {
	// connector is the connector type ID; empty for an agent without a code-host connection.
	connector string
	loc       connectors.Location
	site      codehost.Site
}

// codeHost resolves the host of the agent's code-host binding. An agent with none, or whose
// connection is not found, is on the default host.
func (r *runner) codeHost(ctx context.Context) (codeHost, error) {
	connector, b, ok := providers.Default().Binding(r.vcfg)
	t, found := connectors.Get(connector)
	if !ok || !found || b.ConnectionID == "" {
		return codeHost{loc: connectors.Location{Default: true}}, nil
	}
	var id pgtype.UUID
	if err := id.Scan(b.ConnectionID); err != nil || !id.Valid {
		return codeHost{}, fmt.Errorf("the agent's %s binding names a bad connection ID %q", connector, b.ConnectionID)
	}
	conn, err := r.q.GetConnection(ctx, store.GetConnectionParams{OrgID: r.run.OrgID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		// The binding names a connection that is gone: there is no host to render.
		return codeHost{loc: connectors.Location{Default: true}}, nil
	}
	if err != nil {
		return codeHost{}, fmt.Errorf("read the %s connection: %w", connector, err)
	}
	site, err := codehost.SiteOf(conn.Config)
	if err != nil {
		return codeHost{}, err
	}
	loc, err := site.Location(t)
	if err != nil {
		return codeHost{}, fmt.Errorf("the %s connection: %w", connector, err)
	}
	return codeHost{connector: connector, loc: loc, site: site}, nil
}

// checkCodeHostCredential fails the run at once when the agent's code-host connection lost its
// stored credential with a key (ADR 0085 decision 13): the reconciler cannot mint the sandbox's
// token, so waiting for the sandbox would only end in a timeout that does not say why.
func (r *runner) checkCodeHostCredential(ctx context.Context) error {
	connector, b, ok := providers.Default().Binding(r.vcfg)
	if !ok || b.ConnectionID == "" {
		return nil
	}
	var id pgtype.UUID
	if err := id.Scan(b.ConnectionID); err != nil || !id.Valid {
		return nil // codeHost reports the bad ID
	}
	conn, err := r.q.GetConnection(ctx, store.GetConnectionParams{OrgID: r.run.OrgID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the %s connection: %w", connector, err)
	}
	if conn.AdminDisabledAt.Valid {
		return &failure{reason: fmt.Sprintf("the %s connection %q is disabled by an admin (outward switch); an admin has to enable it, then run the task again",
			connector, conn.DisplayName)}
	}
	if conn.CredentialLostAt.Valid {
		return &failure{reason: fmt.Sprintf("the stored credential of the %s connection %q was lost; %s, then run the task again",
			connector, conn.DisplayName, connections.LostRemedy(conn))}
	}
	return nil
}

// writeGitConfig renders the git configuration of the agent's code host into the sandbox. It is
// written for the default host too, so a connection moved back to github.com loses the old
// host's configuration.
func (r *runner) writeGitConfig(ctx context.Context, workspace, sandbox string) error {
	h, err := r.codeHost(ctx)
	if err != nil {
		return &failure{reason: "git configuration for the code host: " + err.Error()}
	}
	hasCA := strings.TrimSpace(h.site.CABundle) != ""
	dir := dirOf(codehost.SandboxGitConfig)
	_, code, stderr, err := r.sandboxExec(ctx, workspace, sandbox,
		[]string{"/bin/sh", "-c", writeGitScript, "sh", dir, codehost.SandboxGitConfig},
		codehost.GitConfig(h.loc, hasCA))
	if err != nil {
		return err
	}
	if code != 0 {
		return &failure{reason: fmt.Sprintf("write the git configuration to %s: exit %d: %s", codehost.SandboxGitConfig, code, strings.TrimSpace(stderr))}
	}
	script, in := removeCAScript, []byte(nil)
	if hasCA {
		script, in = writeCAScript, []byte(strings.TrimSpace(h.site.CABundle)+"\n")
	}
	_, code, stderr, err = r.sandboxExec(ctx, workspace, sandbox,
		[]string{"/bin/sh", "-c", script, "sh", dir, codehost.SandboxGitCA}, in)
	if err != nil {
		return err
	}
	if code != 0 {
		return &failure{reason: fmt.Sprintf("write the CA bundle to %s: exit %d: %s", codehost.SandboxGitCA, code, strings.TrimSpace(stderr))}
	}
	return nil
}

// origin is the host (and port, when not 443, and the sub-path of a Gitea served under one) that
// git remotes of the connection name; empty for an agent without a code-host connection, which
// savework.sh reads as github.com.
func (h codeHost) origin() string {
	if h.loc.Web.Host == "" {
		return ""
	}
	return h.loc.Web.Authority() + h.loc.WebPath
}
