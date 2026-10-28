// SPDX-License-Identifier: Apache-2.0

import { timestampDate } from '@bufbuild/protobuf/wkt'
import type { Timestamp } from '@bufbuild/protobuf/wkt'
import { TriangleAlert } from 'lucide-react'

const day = 24 * 3600 * 1000

/** tokenAgeDays is the whole days since a token was entered in Koinon. */
export function tokenAgeDays(setAt: Timestamp | undefined, now: Date = new Date()): number | undefined {
  if (!setAt) return undefined
  return Math.max(0, Math.floor((now.getTime() - timestampDate(setAt).getTime()) / day))
}

/**
 * TokenAge shows how old a static token is (ADR 0063): since it was entered in Koinon, because
 * Gitea does not tell when a token was created. After warnAfterSeconds it warns. Nothing blocks
 * on age and nothing rotates the token.
 */
export function TokenAge({ setAt, warnAfterSeconds, now }: { setAt: Timestamp | undefined; warnAfterSeconds: number; now?: Date }) {
  const days = tokenAgeDays(setAt, now)
  if (days === undefined) return <span className="text-muted-foreground">age unknown</span>
  const text = days === 0 ? 'entered today' : `entered ${days} ${days === 1 ? 'day' : 'days'} ago`
  const warnDays = Math.floor(warnAfterSeconds / (day / 1000))
  if (warnAfterSeconds > 0 && days >= warnDays) {
    return (
      <span className="text-destructive inline-flex items-center gap-1" role="status">
        <TriangleAlert aria-hidden className="size-3.5" />
        {text}: older than {warnDays} days. Gitea tokens do not expire, so replace it if you are unsure who has seen it.
      </span>
    )
  }
  return <span>{text}</span>
}
