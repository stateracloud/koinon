# Secret handling

Owner: Security. Applies to every environment, including the home lab and CI.

## Rules

1. Secrets must live in the secret store (Vault in production, SOPS-encrypted files in Git for the
   lab). They must not appear in Git in plain text, container images, build logs or chat.
2. Workloads get secrets at run time as mounted files or injected environment variables from the
   secret store. Baking a secret into an image is a policy violation.
3. Each secret has one owner and one purpose. Sharing one credential between two services is not
   allowed; create a second credential.
4. Humans read production secrets only through the break-glass role (see Access windows) and the
   read is logged.
5. CI tokens must be scoped to one repository and expire within 90 days.

## Rotation

| Secret type | Rotate every | Rotate immediately when |
|---|---|---|
| Database passwords | 90 days | an employee with access leaves |
| API keys for third parties | 180 days | the key appears in a log or ticket |
| Signing keys | 365 days | a host that held it is rebuilt from an unknown state |
| Personal access tokens | 90 days, enforced by expiry | the laptop is lost |

## Found a leaked secret

1. Revoke it first, then clean up. Do not wait for the Git history rewrite.
2. Open an incident at severity 3 (severity 2 if it grants production write access).
3. Scan with `gitleaks git` and record the finding in the incident.

## Checks

- Weekly: the secret scanner runs on every repository; findings page the Security rota.
- Quarterly: Security samples five secrets and confirms owner, purpose and age.
