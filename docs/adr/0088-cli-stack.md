# 0088. The command-line stack: cobra, pflag and viper, with Charm on a terminal only

- Status: Accepted
- Date: 2026-11-01
- Relates to: [0060](0060-migrations-as-a-job.md), [0076](0076-database-modes.md), [0079](0079-dependency-cooldown.md), [0085](0085-upgrade-compatibility-and-recovery-policy.md)

## Context

Koinon's binaries used the standard library `flag` package and `internal/cli.BindEnv`.
`cmd/koinon-api` had about 60 flags, matched its subcommands by hand on `os.Args` (`migrateOnly()`)
and split validation between the flag parser and the middle of `run()`. The reconciler had its own
flag-then-file-then-default precedence, and agentd its own flags. The chart's side of the contract
(which `KOINON_*` variable each Job or Deployment gets) was checked by awk in `hack/chart-check.d`.

Every fix of the kind below was a mismatch between what a binary needs at startup and what the chart
gives it: old issue 284 and 43d2855e (the migrate Job inherited a check meant for the server, so every fresh
install failed), 3151301a (`KOINON_KEK_DIR` was missing from the reconciler) and 8c5c8852
(precedence). The binaries and the chart need one contract, written once and tested against what
`helm template` renders.

## Decision

1. **Stack.** cobra (commands), pflag (flags) and viper (sources) in every Koinon binary, and Charm
   for presentation: fang for styled help, usage, errors and version, lipgloss for the operator
   subcommands' output. Interactive prompts (huh) are covered by "Interactive prompts" below.
2. **viper owns the sources.** A setting comes from, highest first: the flag, the environment, the
   config file (for the binaries that read one), the default. The environment variable of a flag is
   `KOINON_` plus the flag name in upper case with `-` as `_`; a flag may declare aliases that are
   read after it (`DATABASE_URL`, `POD_NAMESPACE`). An empty variable counts as set, so
   `KOINON_OTLP_ADDR=` turns the receiver off as `--otlp-addr=` does. Config file keys are the flag
   names, or the nested key a flag declares with `cli.ConfigKey` (the reconciler's `--gateway-mode`
   is `gateway.mode` in the chart's `config.yaml`, read through `cli.ConfigFile`); an empty value in
   the file leaves the default, as the chart renders an unset value that way. Every existing variable and file key keeps its name, so no chart value changes and no
   upgrade breaks (ADR 0085). `internal/cli` implements this once (`Bind`, `Resolve`), replacing
   `BindEnv` and the reconciler's `fill`.
3. **Each command declares its own flags and what it requires.** Required flags are marked required
   and may come from the environment (the sources resolve before cobra checks them); rules between
   flags live in the command's `PreRunE`. A check only the server needs lives on `serve`, so `migrate`
   cannot inherit it (ADR 0060 keeps `migrate` minimal). koinon-api's commands are `serve`,
   `migrate`, `preflight`, `ensure-secrets`, `audit` and `keys`. Without a subcommand koinon-api
   serves, so a pod configured by environment alone still starts.
4. **The command tree is importable.** A binary's tree lives in `internal/cmd/<binary>` and
   `cmd/<binary>/main.go` only calls it, so a test can build it.
5. **The chart contract test** (`test/chartsmoke`, `TestChartContract`, plain `go test`, needs only
   helm) renders the chart for every `charts/koinon/ci` values file and, for each container of a
   binary with a registered tree, runs that binary's real startup without its work
   (`cli.Check`): the command exists, every flag is known and parses, every required flag is present
   by flag or environment, the `PreRunE` rules hold, and no `KOINON_*` variable is set that the command
   does not read (a binary declares the ones it reads outside flags with `cli.AllowEnv`). Values that
   come from Secrets or the downward API are placeholders. `make test-chart-smoke` still boots the
   containers. A binary joins the test by adding its tree to `commandTrees`.
6. **Plain unless a terminal.** Colour and layout appear only when stdout and stderr are terminals
   and `NO_COLOR` is unset (`cli.Styled`). In pods and CI help and errors are plain text, and a
   failed start is one structured slog line.
7. **No behaviour change.** The same flags, variables, defaults and exit codes, with two spelling
   changes that follow from pflag: long flags take two dashes (`-durable` is `--durable`), and flags
   follow the subcommand (`koinon-api migrate --wait 5m`, no longer `koinon-api -wait 5m migrate`).
   The chart passes the new spelling; nothing else in the tree used the old one.

## Interactive prompts

(Decided 2026-11-02, old issue 459.) An operator subcommand may ask a person on a terminal for what the flags
and arguments left out, with huh (`internal/cli`: `Interactive`, `Prompter`).

- **A prompt is a convenience over the flag form, never a replacement.** The gate is `cli.Interactive`:
  stdin and stdout are terminals and `--no-input` is off. Otherwise a command behaves exactly as before
  (same errors, no confirmations, no `--yes` needed), so a pod, a script or CI never waits on input.
- **`--no-input`** is a persistent flag on every binary's root (`cli.Bind`), also `KOINON_NO_INPUT`. The
  chart does not set it: a container has no terminal anyway.
- **Accessible mode.** `TERM=dumb` uses huh's line prompts. Colour follows `cli.Styled` and `NO_COLOR`.
- **Scope.** Only commands a person runs by hand ask: `koinon-api keys activate|retire|rewrap|reencrypt`.
  `--yes` skips their confirmations. Hook Jobs, servers and reconcilers never prompt.

## Alternatives considered

- **Keep `flag` and grow `BindEnv`.** Each binary keeps its own precedence and its own validation
  placement; the mismatch class stays. Cheaper now, repeated per binary later.
- **kong or urfave/cli.** Comparable features, but cobra and viper are what the Kubernetes tooling
  around Koinon (kubectl, helm, the operators) use, so contributors already know them.
- **Contract test by running the images.** `make test-chart-smoke` does, and stays; it needs Docker,
  a cluster API and minutes. The command-tree test runs in `make test` in a second.
- **Accept single-dash long flags.** pflag reads `-wait` as `-w -a -i -t`; a shim that rewrites
  argv would hide the break instead of removing it.

## Consequences

- Positive: one precedence rule, tested once; a flag or variable the chart gets wrong fails
  `make test`, not a fresh install.
- Negative: a larger dependency set (cobra, viper, fang and their transitive modules, all past the
  ADR 0079 age check); the old argument order stops working.
- Follow-ups: agentd (old issue 458) moves onto `internal/cli` and joins `commandTrees` (the reconciler did in
  old issue 457, and the contract test writes the ConfigMap it mounts to disk, so its file settings are checked); the interactive prompts (old issue 459); operator subcommand output with lipgloss; moving
  the remaining startup-input assertions of `hack/chart-check.d` into the contract test.

## Links

- Old issue 456 and the 2026-11-01 architecture review.
- Dependency age check: `hack/dep-age-check.sh`.
