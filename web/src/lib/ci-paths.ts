// SPDX-License-Identifier: Apache-2.0

/**
 * The paths whose changes run CI on a code host (ADR 0062). The API returns them with the approval
 * (ciPaths, ciFoldCase): the connector's own CI paths plus the connection's configured directories.
 * An entry guards itself and everything under it; matching is exact unless the provider ignores case.
 * This mirrors the Go workflow guard (internal/codehost/guard.go), which is what refuses a change.
 */

/** Cleans a path the way the guard does: separators unified, leading ./ and / dropped, . and .. resolved. */
export function cleanPath(p: string): string {
  const out: string[] = []
  for (const seg of p.replaceAll('\\', '/').split('/')) {
    if (seg === '' || seg === '.') continue
    if (seg === '..') out.pop()
    else out.push(seg)
  }
  return out.join('/')
}

/** Returns the guarded entry that p falls under, or undefined. */
export function ciPathFor(p: string, ciPaths: readonly string[], foldCase: boolean): string | undefined {
  const fold = (s: string) => (foldCase ? s.toLowerCase() : s)
  const c = fold(cleanPath(p))
  return ciPaths.find((entry) => {
    const d = fold(cleanPath(entry))
    return d !== '' && (c === d || c.startsWith(d + '/'))
  })
}

/** The distinct guarded entries the paths fall under, in the order of ciPaths. */
export function ciPathsHit(paths: readonly string[], ciPaths: readonly string[], foldCase: boolean): string[] {
  const hit = new Set(paths.map((p) => ciPathFor(p, ciPaths, foldCase)).filter((e): e is string => !!e))
  return ciPaths.filter((e) => hit.has(e))
}

/** Splits the form's textarea into directories, one per line. */
export function parseCiDirs(text: string): string[] {
  return text
    .split(/[\r\n,]+/)
    .map((s) => s.trim())
    .filter(Boolean)
}

/** A message for the first directory the API would refuse, or undefined. */
export function ciDirsError(text: string): string | undefined {
  const dirs = parseCiDirs(text)
  if (dirs.length > 20) return 'At most 20 directories.'
  for (const d of dirs) {
    if (d.startsWith('/')) return `${d} is absolute; write it relative to the repository root.`
    if (d.includes('\\')) return `${d} contains a backslash; use / as the separator.`
    if (d.split('/').includes('..')) return `${d} leaves the repository.`
    if (cleanPath(d) === '') return `${d} is the repository root; list the directories that hold CI configuration.`
  }
  return undefined
}
