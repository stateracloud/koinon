---
title: OIDC login
description: Sign in to Koinon with your identity provider.
---

Koinon logs users in with any OpenID Connect provider (authorization code with PKCE): Entra ID, Okta,
Keycloak, Dex or Google. Roles live in Postgres, not in the provider. Group mappings can set the roles
in the default org
([ADR 0004](https://github.com/stateracloud/koinon/blob/main/docs/adr/0004-human-identity.md)).

## Register Koinon with your provider

Create a web application in the provider. Set its redirect URL to `<publicUrl>/auth/oidc/callback`,
for example `https://koinon.example.com/auth/oidc/callback`. Note the issuer URL, the client ID and
the client secret. Have the provider put a `groups` claim in the ID token if you want group mappings.

## Configure the chart

Store the client secret:

```sh
kubectl -n koinon create secret generic koinon-oidc --from-literal=client-secret="$OIDC_CLIENT_SECRET"
```

Then set the values:

```yaml chart-values
publicUrl: https://koinon.example.com

oidc:
  enabled: true
  issuerUrl: https://idp.example.com/realms/koinon
  clientId: koinon
  clientSecret:
    existingSecret: koinon-oidc
    key: client-secret
  scopes: [email, profile, groups]
  groupsClaim: groups
  groupMappings:
    koinon-admins: admin
    staff: member
  platformAdminGroups: [koinon-platform]
  defaultOrgIdentityMode: hybrid
```

- `publicUrl` is the address browsers use. Its scheme sets the session cookie's `Secure` flag. The
  default redirect URL is built from it. Set `oidc.redirectUrl` to use another.
- Leave `clientSecret.existingSecret` empty for a public client (PKCE only).
- `groupMappings` map an IdP group to a role in the default org: `viewer`, `member`, `admin` or `owner`.
  The chart reconciles them on every start.
- `platformAdminGroups` lists IdP groups whose members are platform admins (they set the per-org budget
  ceilings under `/platform/budgets`). It is reconciled at every login like the group mappings: a user
  who gains a listed group becomes a platform admin, a user who loses all of them stops being one. Only
  grants made by this setting are revoked, so the bootstrap admin keeps its own. An empty list changes
  nothing. Group names may contain spaces, not commas. If the IdP omits the groups claim from the ID token
  (a missing `groups` scope, or the Azure AD groups overage, where the token carries `_claim_names`
  instead of the list), the user counts as in no group and loses the grant at the next login, the same
  as with `groupMappings`. Koinon only reads groups at login, so a user who is removed
  from the IdP and never logs in again keeps platform admin. To remove one at once, delete the row
  (`DELETE FROM platform_admins WHERE user_id = '<id>'`, in Postgres) and disable the user (set
  `users.deleted_at`), which also blocks the login. Grants and revocations the login makes are audited
  as `platform_admin.grant` and `platform_admin.revoke` in the install audit log (actor `system koinon-api:oidc-sync`).
- `defaultOrgIdentityMode` is `platform` (roles from Koinon only), `idp` (from `groupMappings`) or
  `hybrid` (both, and the highest role wins).

The login page then shows **Sign in with SSO**. Sessions are HTTP-only cookies backed by a server-side
table. Logging out revokes the session.

## Group changes and sessions

Koinon reads groups at login. A user removed from an IdP group keeps the role until the next login or
until the session ends. `session.maxLifetime` (12 hours by default) bounds that time. `session.idleTimeout`
(2 hours by default) ends idle sessions.

## Turn off the bootstrap admin

The bootstrap admin is the only platform admin until you set `oidc.platformAdminGroups` (above) and
a user in one of those groups has signed in once through OIDC. Do that first, and check that the user
can open `/platform/budgets`. Without a platform admin nobody can set the platform budget ceilings, and
the only way back is to enable the bootstrap admin again.

When OIDC works, an OIDC user is an owner and an OIDC user is a platform admin, turn off the password
login:

```yaml chart-values
bootstrapAdmin:
  enabled: false
```

This turns off the API's password login and revokes the bootstrap sessions. Do not do it before you
have signed in once through OIDC and checked your role.
