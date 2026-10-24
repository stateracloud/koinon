// SPDX-License-Identifier: Apache-2.0

// Exact USD arithmetic on the strings the API sends. Amounts are decimals with at most 12 fraction digits
// (pico-USD, internal/money); BigInt keeps sums and ratios exact.

const scale = 12
const unit = 10n ** BigInt(scale)
const amountPattern = /^[0-9]{1,7}(\.[0-9]{1,12})?$/

/** The largest cap or limit the server accepts: 1,000,000 USD. */
export const maxAmountUSD = '1000000'

/** parsePico reads "12.50" as pico-USD, or undefined when it is not an amount the server accepts. */
export function parsePico(s: string): bigint | undefined {
  const v = s.trim()
  if (!amountPattern.test(v)) return undefined
  const [whole, frac = ''] = v.split('.')
  return BigInt(whole) * unit + BigInt(frac.padEnd(scale, '0'))
}

/** formatPico writes pico-USD as a decimal with at least two fraction digits and no trailing zeros beyond that. */
export function formatPico(p: bigint): string {
  const neg = p < 0n
  const abs = neg ? -p : p
  const whole = abs / unit
  const frac = (abs % unit).toString().padStart(scale, '0').replace(/0+$/, '').padEnd(2, '0')
  return `${neg ? '-' : ''}${whole}.${frac}`
}

/** remaining is limit minus spent, never below zero. Undefined when either is not an amount. */
export function remaining(limit: string, spent: string): string | undefined {
  const l = parsePico(limit)
  const s = parsePico(spent)
  if (l === undefined || s === undefined) return undefined
  return formatPico(l > s ? l - s : 0n)
}

/** percent is part of whole in percent (one decimal), or undefined when whole is zero or either is not an amount. */
export function percent(part: string, whole: string): number | undefined {
  const p = parsePico(part)
  const w = parsePico(whole)
  if (p === undefined || w === undefined || w === 0n) return undefined
  return Number((p * 1000n) / w) / 10
}

/** amountError checks a cap or limit the way the server does: a positive decimal of at most 1,000,000 USD. */
export function amountError(label: string, value: string): string | undefined {
  const p = parsePico(value)
  if (p === undefined) return `${label} must be a dollar amount such as 50 or 12.50 (up to 12 decimals).`
  if (p === 0n) return `${label} must be more than 0.`
  if (p > parsePico(maxAmountUSD)!) return `${label} can be at most $1,000,000.`
  return undefined
}
