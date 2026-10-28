// SPDX-License-Identifier: Apache-2.0

const maxCaBundleBytes = 256 << 10

/** caBundleError mirrors the API: PEM CERTIFICATE blocks only, at most 256 KiB. */
export function caBundleError(raw: string): string | undefined {
  const v = raw.trim()
  if (!v) return undefined
  if (new TextEncoder().encode(v).length > maxCaBundleBytes) return 'CA bundle is larger than 256 KiB.'
  const blocks = [...v.matchAll(/-----BEGIN ([A-Z0-9 ]+)-----/g)].map((m) => m[1])
  if (!blocks.length) return 'CA bundle must be PEM certificates (-----BEGIN CERTIFICATE-----).'
  const other = blocks.find((b) => b !== 'CERTIFICATE')
  if (other) return `CA bundle holds a "${other}" block; only certificates are allowed.`
  return undefined
}
