The root AGENTS.md applies too; this file adds rules for `charts/`.

## Render rules

- Chart renders are deterministic: no Helm `lookup` and no random values in templates. Generated
  secrets come from the ensure-secrets hook Job, with one exception: koinon-api creates the KEK Secret
  on its first start (ADR 0056, amendment 2026-11-11), because only it can see the database and
  refuse a new key when encrypted values exist; a native ValidatingAdmissionPolicy guards that
  Secret.
- Objects the reconciler owns are created by the reconciler.
- Every image reference goes through `koinon.imageRef` and every pod spec gets
  `koinon.imagePullSecrets` (ADR 0061).
- Values files without an OpenShell client TLS Secret must set `openshell.insecure: true`.

## Tests

- Run `hack/chart-check.sh`. Add an assertion for every chart behaviour you change, in the owning
  file of `hack/chart-check.d/`; a new feature adds a new file `NN-<feature>.sh`.
- Anything that changes how a Koinon binary starts (a flag or env var it requires, a mount, a Secret
  key, an RBAC rule) is covered by `make test-chart-smoke`; extend it when you add a component or a
  startup input (old issue 284).
- Sandbox admission policy (`charts/koinon/templates/sandbox-policy.yaml`): `make test-admission`.
  Its OpenShell cases run on kind: `make dev-kind-up`, then `make test-escape-kind`.
- Admission policy of the Substrate driver's two namespaces (`charts/koinon/templates/substrate-policy.yaml`, ADR 0120 section 6): the
  `escape admission substrate` specs in `make test-admission`. A change to it needs an assertion in
  `hack/chart-check.d/44-substrate-worker.sh`.
- Details: `docs/dev/ci.md#chart-and-admission-tests`.
