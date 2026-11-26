// SPDX-License-Identifier: Apache-2.0

// The last org used lasts in the browser, so a user lands in it after the next login. The org that a platform admin
// acts in lasts for the tab only: a reload keeps the banner, a new tab or a new login does not (ADR 0126 section 4).
export const lastOrgKey = 'koinon.org'
export const actingKey = 'koinon.actingOrg'

export function read(storage: () => Storage, key: string): string | undefined {
  try {
    return storage().getItem(key) ?? undefined
  } catch {
    return undefined
  }
}

export function write(storage: () => Storage, key: string, value: string | undefined) {
  try {
    if (value === undefined) storage().removeItem(key)
    else storage().setItem(key, value)
  } catch {
    // private mode: the value lasts for this page only
  }
}

export const local = () => localStorage
export const session = () => sessionStorage

/** forgetActingOrg ends the acting state of the tab. Sign out calls it, so the next login lands in the last org. */
export function forgetActingOrg() {
  write(session, actingKey, undefined)
}
