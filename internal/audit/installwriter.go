// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/db/migrations"
)

// installWriterOK is 1 while koinon-api's own database login inherits the install audit writer role,
// 0 otherwise (ADR 0100 decision 1.1a). It has no labels (ADR 0023). It is 0 before the first check
// and keeps its value when a check cannot reach the database.
var installWriterOK = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "koinon_install_audit_writer_ok",
	Help: "1 when koinon-api's database login inherits the install audit writer role (it can write the install audit chain), 0 otherwise.",
})

// Row is what InstallWriterCheck needs of a pool or a connection.
type Row interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// WriterStatus is a login's standing in the install audit writer role.
type WriterStatus struct {
	// User is the login the check ran as (current_user): the role a GRANT must name.
	User string
	// RoleExists reports whether the writer role exists.
	RoleExists bool
	// Member reports whether User inherits the writer role's privileges: a member without INHERIT
	// does not, since koinon-api inserts without SET ROLE.
	Member bool
}

// OK reports whether the login can write the install audit chain through the writer role.
func (s WriterStatus) OK() bool { return s.RoleExists && s.Member }

// CheckInstallWriter returns the standing of db's own login in the writer role. A missing role is a
// status, not an error: pg_has_role of a NULL role is NULL.
func CheckInstallWriter(ctx context.Context, db Row) (WriterStatus, error) {
	var s WriterStatus
	err := db.QueryRow(ctx, `SELECT current_user::text, r.oid IS NOT NULL, COALESCE(pg_has_role(current_user, r.oid, 'USAGE'), false)
		FROM (SELECT 1) AS one LEFT JOIN pg_roles AS r ON r.rolname = $1`, migrations.InstallAuditWriterRole).Scan(&s.User, &s.RoleExists, &s.Member)
	if err != nil {
		return WriterStatus{}, fmt.Errorf("check the install audit writer role: %w", err)
	}
	return s, nil
}

// InstallWriterCheck is koinon-api checking its own login against the writer role (maintainer
// decision 2026-11-07): only the process knows the login it connects as. It sets
// koinon_install_audit_writer_ok and logs one line per change of state, with the fix filled in. It
// never fails the start: koinon-api serves without the grant (ADR 0100 decision 1.1a).
type InstallWriterCheck struct {
	DB  Row
	Log *slog.Logger
	// Timeout bounds one check; zero means 5 seconds, as WarnSharedWriterLogin waits.
	Timeout time.Duration
	// state is the last state the database answered with ("" before the first answer); an error
	// does not change it, so ok, error, ok is not a new membership.
	state string
	// errLogged is true while the last check failed and said so.
	errLogged bool
}

// Check runs the check once, waiting at most Timeout, and reports the status (the zero status when the database could not be
// asked).
func (c *InstallWriterCheck) Check(ctx context.Context) WriterStatus {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	s, err := CheckInstallWriter(cctx, c.DB)
	if err != nil {
		if ctx.Err() == nil && !c.errLogged {
			c.errLogged = true
			c.Log.Warn("cannot check koinon-api's install audit writer membership; retrying", "err", err)
		}
		return WriterStatus{}
	}
	c.errLogged = false
	state := "ok"
	switch {
	case !s.RoleExists:
		state = "no-role"
	case !s.Member:
		state = "not-member"
	}
	if s.OK() {
		installWriterOK.Set(1)
	} else {
		installWriterOK.Set(0)
	}
	if state == c.state {
		return s
	}
	first := c.state == ""
	c.state = state
	grant := fmt.Sprintf("GRANT %s TO %s", migrations.InstallAuditWriterRole, pgx.Identifier{s.User}.Sanitize())
	switch state {
	case "no-role":
		c.Log.Error("the role "+migrations.InstallAuditWriterRole+" does not exist, so koinon-api records no install audit event; "+
			"with the chart's CloudNativePG cluster its post-upgrade hook waits for it; otherwise, as an administrator, run: "+
			"CREATE ROLE "+migrations.InstallAuditWriterRole+" NOLOGIN; "+grant+"; then koinon-api migrate --grants-only "+
			"(docs/ops/secret-keys.md, The install audit chain)", "login", s.User)
	case "not-member":
		c.Log.Error("koinon-api's database login is not a member of "+migrations.InstallAuditWriterRole+
			" (or does not inherit it), so the database refuses its install audit events; as an administrator run: "+grant+
			" (docs/ops/secret-keys.md, The install audit chain)", "login", s.User)
	default:
		if first {
			c.Log.Info("koinon-api's database login can write the install audit chain", "login", s.User, "role", migrations.InstallAuditWriterRole)
		} else {
			c.Log.Info("koinon-api's database login is now a member of the install audit writer role", "login", s.User)
		}
	}
	return s
}

// WarnSharedWriterLogin logs a warning when a component other than koinon-api (component names it)
// connects as a login that inherits the install audit writer role: then it shares koinon-api's login,
// or a role with its privilege, and could append install events under any actor (docs/ops/secret-keys.md).
// It never fails the start, and waits at most 5 seconds; a database it cannot ask is ignored, the
// component's own checks see it.
func WarnSharedWriterLogin(ctx context.Context, db Row, component string, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s, err := CheckInstallWriter(ctx, db)
	if err != nil || !s.OK() {
		return
	}
	log.Warn(component+"'s database login can write the install audit chain: it shares koinon-api's login or a role "+
		"with its privilege, so it could append install events under any actor; give "+component+" a login of its own, "+
		"not in "+migrations.InstallAuditWriterRole+" (docs/ops/secret-keys.md, The install audit chain)", "login", s.User)
}

// Run checks every interval until ctx ends, after the check the caller made at startup: a grant, or
// a revoke, shows within one interval. Not safe for concurrent use with Check.
func (c *InstallWriterCheck) Run(ctx context.Context, interval time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		c.Check(ctx)
	}
}
