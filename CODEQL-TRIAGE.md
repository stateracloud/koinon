# CodeQL triage (#152)

## Alert #3 go/clear-text-logging, internal/adminapi/adminapi.go:156

Every non-AuthError path of `tokens.Authenticate` (internal/tokens/tokens.go), and what each error can carry:

- L317-319 malformed token: returns `*AuthError` (handled before the log line).
- L323 `GetAPITokenByPrefix(ctx, prefix)` ErrNoRows: `*AuthError`, no raw text.
- L329-331 same query, any other error: a pgx/pgconn or net/context error. The only query argument is the 8-character public prefix, never the raw token or header; the raw token is only hashed (L326, L334).
- L341-342 `holdsRole` (L359-373): errors from `IsPlatformAdmin` / `ListEffectiveMemberships`, whose arguments are UUIDs.
- L347-350 and every reason: `*AuthError`, `Error()` returns the constant `ErrInvalid` text (L310).

So no path puts the raw token or header into an error today. The data flow is still tainted for CodeQL, and a future query change could add an argument. Fix: adminapi.go now logs `error_class` (`canceled`, `timeout`, `database_error_<SQLSTATE>`, `database_unreachable`, `internal`) through `errorClass`, never the error text. Test: internal/adminapi/tokenlog_test.go makes the token check fail with a dead database and asserts neither the token nor "Bearer" reaches the log or response.

## Dismissal reasons (maintainer dismisses in code scanning)

- #6 go/command-injection, internal/testing/fakegitea/git.go:85: test-only fake Gitea server; the executable is the fixed `git` binary and arguments come from the test's own fixtures, never from network input. Reason: used in tests.
- #4 weak-sensitive-data-hashing, providers.go:175: the hash only derives a 4-byte filename suffix for uniqueness; it protects nothing and the input is not a credential. Reason: false positive.
- #2 incorrect-integer-conversion, urlpolicy.go:200: the conversion happens after an explicit bound check that keeps the value within the target type's range. Reason: false positive.
- #5 unvalidated-url-redirection, oidctest/issuer.go:149: test OIDC issuer under internal/testing, started only by tests on loopback; the redirect target is test-supplied. Reason: used in tests.
