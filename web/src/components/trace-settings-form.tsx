// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useState, type FormEvent } from 'react'
import { FullContentBadge } from '@/components/trace-destination-card'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { ContentLevel, type Settings } from '@/gen/koinon/trace/v1alpha1/trace_pb'
import { clampLevel, levelDisabledReason, levelLabel, levelRank, levels, normalLevel, percentToRate, rateToPercent, samplingError } from '@/lib/trace'

export type TraceSettingsFormProps = {
  settings: Settings
  /** Only an org owner may choose full. */
  isOwner: boolean
  save: (contentLevel: ContentLevel, sampleRate: number, platformCopyLevel: ContentLevel) => Promise<unknown>
  onSaved?: () => void
}

/**
 * TraceSettingsForm sets what the org's traces carry (the content level), how many runs are traced,
 * and, when the platform has a destination, what the platform's copy may carry. A level above the
 * platform ceiling cannot be picked, and only an owner can pick full, after a confirmation.
 */
export function TraceSettingsForm({ settings, isOwner, save, onSaved }: TraceSettingsFormProps) {
  const ceiling = normalLevel(settings.ceiling)
  const stored = normalLevel(settings.contentLevel)
  const [level, setLevel] = useState(() => clampLevel(stored, ceiling))
  const [percent, setPercent] = useState(() => rateToPercent(settings.sampleRate))
  const [copyChoice, setCopyChoice] = useState(normalLevel(settings.platformCopyLevel))
  const [confirming, setConfirming] = useState(false)
  const [pending, setPending] = useState(false)
  const [refused, setRefused] = useState<string>()
  const [saved, setSaved] = useState(false)
  // The platform's copy never carries more than the org's own level.
  const copy = clampLevel(copyChoice, level)
  const copyReason = (l: ContentLevel) => levelDisabledReason(l, ceiling, isOwner, settings.platformCopyLevel)
  const samplingProblem = samplingError(percent)

  async function commit() {
    const rate = percentToRate(percent)
    if (rate === undefined) return
    setPending(true)
    setRefused(undefined)
    setSaved(false)
    try {
      await save(level, rate, settings.hasPlatformDestination ? copy : normalLevel(settings.platformCopyLevel))
      setConfirming(false)
      setSaved(true)
      onSaved?.()
    } catch (e) {
      setConfirming(false)
      setRefused(ConnectError.from(e).rawMessage)
    } finally {
      setPending(false)
    }
  }

  function submit(e: FormEvent) {
    e.preventDefault()
    if (samplingProblem) return
    if (level === ContentLevel.FULL && stored !== ContentLevel.FULL) {
      setConfirming(true)
      return
    }
    void commit()
  }

  return (
    <form onSubmit={submit} aria-label="Trace settings" className="bg-card grid gap-4 rounded-md border p-4">
      <div className="flex flex-wrap items-center gap-2">
        <h2 className="text-base font-semibold">What traces carry</h2>
        {stored === ContentLevel.FULL && <FullContentBadge />}
      </div>
      <fieldset className="grid gap-2">
        <legend className="text-sm font-medium">Content level</legend>
        <p className="text-muted-foreground text-xs">
          The platform ceiling is <strong>{levelLabel(ceiling)}</strong>: no org can send more than that.
        </p>
        {levels.map((l) => {
          const reason = levelDisabledReason(l.value, ceiling, isOwner, stored)
          return (
            <label key={l.value} className="flex items-start gap-2 text-sm">
              <input
                type="radio"
                name="content-level"
                className="mt-1"
                value={l.value}
                checked={level === l.value}
                disabled={!!reason || pending}
                aria-describedby={`level-${l.value}-help`}
                onChange={() => {
                  setLevel(l.value)
                  setSaved(false)
                }}
              />
              <span>
                <span className="font-medium">{l.label}</span>
                <span id={`level-${l.value}-help`} className="text-muted-foreground block text-xs">
                  {l.help}
                  {reason && ` ${reason}`}
                </span>
              </span>
            </label>
          )
        })}
      </fieldset>
      <div className="grid gap-1.5">
        <label htmlFor="trace-sampling" className="text-sm font-medium">
          Share of runs traced (%)
        </label>
        <Input
          id="trace-sampling"
          className="max-w-32"
          inputMode="decimal"
          value={percent}
          onChange={(e) => {
            setPercent(e.target.value)
            setSaved(false)
          }}
          aria-invalid={!!samplingProblem}
          aria-describedby="trace-sampling-help"
        />
        <p id="trace-sampling-help" className="text-muted-foreground text-xs">
          100 traces every run. A run is traced whole or not at all, decided when it starts.
        </p>
        {samplingProblem && (
          <p role="alert" className="text-destructive text-xs">
            {samplingProblem}
          </p>
        )}
      </div>
      {settings.hasPlatformDestination && (
        <div className="grid gap-1.5">
          <label htmlFor="trace-platform-copy" className="text-sm font-medium">
            Content in the platform&apos;s copy
          </label>
          <NativeSelect id="trace-platform-copy" value={String(copy)} onChange={(e) => setCopyChoice(Number(e.target.value) as ContentLevel)} aria-describedby="trace-platform-copy-help">
            {levels.map((l) => (
              <option key={l.value} value={l.value} disabled={levelRank(l.value) > levelRank(level) || !!copyReason(l.value)}>
                {l.label}
              </option>
            ))}
          </NativeSelect>
          <p id="trace-platform-copy-help" className="text-muted-foreground text-xs">
            The platform operator also receives a copy of your traces. By default it carries no content. You can let it carry up to your own level, never more.
          </p>
        </div>
      )}
      {confirming && (
        <Alert role="alertdialog" aria-label="Confirm full content">
          <AlertDescription className="grid gap-2">
            <span>
              Send full content? Prompts, replies, tool arguments and results, and agent messages go to your destinations as they are, apart from credentials. Anyone with access to those backends can read them. The change is recorded in the audit log.
            </span>
            <span className="flex gap-2">
              <Button type="button" variant="destructive" size="sm" disabled={pending} onClick={() => void commit()}>
                Send full content
              </Button>
              <Button type="button" variant="outline" size="sm" disabled={pending} onClick={() => setConfirming(false)}>
                Cancel
              </Button>
            </span>
          </AlertDescription>
        </Alert>
      )}
      {refused && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{refused}</AlertDescription>
        </Alert>
      )}
      {saved && (
        <p role="status" className="text-sm">
          Saved.
        </p>
      )}
      <div className="flex justify-end">
        <Button type="submit" disabled={pending || confirming}>
          Save settings
        </Button>
      </div>
    </form>
  )
}
