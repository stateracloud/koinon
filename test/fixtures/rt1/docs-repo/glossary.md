# Glossary

Terms used in the Northwind platform handbook.

| Term | Meaning |
|---|---|
| Break-glass | Time-limited, logged production access granted during an incident. |
| Change window | Hours in which normal production changes are allowed. See Access windows. |
| Freeze | A period when only severity 1 fixes may ship. |
| IC | Incident commander. Runs an incident and does not debug. |
| On-call | The engineer who answers pages for a week. |
| Pinned | Fixed to an exact version or digest, so a rebuild gets the same bits. |
| RPO | Recovery point objective: the most data we accept losing. |
| RTO | Recovery time objective: the longest we accept being down. |
| Restore log | The table recording each restore test and its outcome. |
| Secret store | Vault in production, SOPS-encrypted files in Git for the test cluster. |
| Severity | Incident level 1 to 4; 1 is the worst. |
| Tier | A service's importance class; sets its RPO and RTO. |
| WAL | Write-ahead log of a database, shipped for point-in-time recovery. |
