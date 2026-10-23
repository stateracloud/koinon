// SPDX-License-Identifier: Apache-2.0

// The push-and-run risk the approval card leads with (ADR 0049, plan item
// W2). Approving a pull request lets the change into the repository, where
// CI runs it with the repository's secrets before any GitHub review, so the
// card puts what runs (CI config, scripts, files made executable), what pulls
// in code (dependency manifests), what cannot be read (binaries) and how much
// changes first.

import type { Risk } from '@/gen/koinon/approval/v1alpha1/approval_pb'

export type RiskKey = 'ci' | 'scripts' | 'executable' | 'dependencies' | 'binary' | 'deletions' | 'size'

export type RiskItem = {
  key: RiskKey
  label: string
  /** Why it matters, in one line. */
  why: string
  paths: readonly string[]
  /** Flagged items are shown as warnings; the rest as plain facts. */
  flagged: boolean
}

/** Warn about size at half the tool's hard cap (100 files or 1 MiB). */
export const sizeWarnFiles = 50
export const sizeWarnBytes = 512 * 1024

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`
  return `${(n / (1024 * 1024)).toFixed(2)} MiB`
}

/**
 * riskItems orders the summary by how directly a change can run code with
 * the repository's secrets: CI configuration, then scripts, files made
 * executable, dependency manifests, binaries, deletions, and the change size
 * last. Every category
 * is listed, so "none" is an explicit answer too.
 */
export function riskItems(risk: Risk | undefined): RiskItem[] {
  const r = risk ?? { files: 0n, bytes: 0n, ci: [], scripts: [], executable: [], dependencies: [], binary: [], deletions: [] }
  const files = Number(r.files)
  const bytes = Number(r.bytes)
  return [
    { key: 'ci', label: 'CI and automation config', why: 'Runs in CI with the repository’s secrets.', paths: r.ci, flagged: r.ci.length > 0 },
    { key: 'scripts', label: 'Scripts and build files', why: 'Executed by tooling, CI or people.', paths: r.scripts, flagged: r.scripts.length > 0 },
    { key: 'executable', label: 'Makes executable', why: 'Written as mode 100755, so it can be run directly.', paths: r.executable, flagged: r.executable.length > 0 },
    {
      key: 'dependencies',
      label: 'Dependency manifests',
      why: 'Pull new code into builds and CI.',
      paths: r.dependencies,
      flagged: r.dependencies.length > 0,
    },
    { key: 'binary', label: 'Binary files', why: 'Cannot be reviewed as text.', paths: r.binary, flagged: r.binary.length > 0 },
    { key: 'deletions', label: 'Deleted files', why: 'Removed from the repository.', paths: r.deletions, flagged: r.deletions.length > 0 },
    {
      key: 'size',
      label: 'Change size',
      why: `${files} ${files === 1 ? 'file' : 'files'}, ${formatBytes(bytes)}`,
      paths: [],
      flagged: files > sizeWarnFiles || bytes > sizeWarnBytes,
    },
  ]
}

/** riskFlagsFor lists the risk categories a path falls in, for the file list. */
export function riskFlagsFor(path: string, items: readonly RiskItem[]): RiskItem[] {
  return items.filter((i) => i.paths.includes(path))
}
