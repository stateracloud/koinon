// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { useMemo, useState } from 'react'
import { closeOnly } from '@/lib/editing'
import { ConfirmAction } from '@/components/confirm-action'
import { PriceForm } from '@/components/price-form'
import { RequireAdmin } from '@/components/require-admin'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import { PriceService, type PriceEntry, type PriceVersion } from '@/gen/koinon/budget/v1alpha1/price_pb'
import { usePriceActions } from '@/hooks/use-spend-actions'
import { useOrg } from '@/lib/org-context'
import {
  changeById,
  changeLabel,
  filterFlagged,
  flagReasons,
  formatWhen,
  groupPrices,
  matchLabel,
  ratesLine,
  reasonsOf,
  safeSource,
  sinceChoices,
  sinceTimestamp,
  type FlagReason,
} from '@/lib/prices'
import { formatUSD } from '@/lib/cost'

/** PricesPage is where org admins and owners see the default prices and manage their own (ADR 0056, plan item PV2). */
export function PricesPage() {
  return (
    <div className="mx-auto grid max-w-3xl grid-cols-[minmax(0,1fr)] gap-6">
      <RequireAdmin what="prices">
        <PricesContent />
      </RequireAdmin>
    </div>
  )
}

function PricesContent() {
  const { org } = useOrg()
  const orgId = org.orgId
  const actions = usePriceActions(orgId)
  const [adding, setAdding] = useState(false)
  const [editing, setEditing] = useState<string>()
  const [notice, setNotice] = useState<string>()
  const [refused, setRefused] = useState<string>()
  const [warnings, setWarnings] = useState<string[]>([])
  const prices = useQuery(PriceService.method.listPrices, { orgId })
  const groups = useMemo(() => groupPrices(prices.data?.prices ?? []), [prices.data])
  const saved = (name: string, warns: string[]) => {
    setRefused(undefined)
    setWarnings(warns)
    setNotice(`Saved ${name}. It applies to new requests from its start date.`)
  }

  return (
    <>
      <div className="flex items-center gap-3">
        <h1 className="text-xl font-semibold">Prices</h1>
        <Button size="sm" className="ml-auto" onClick={() => setAdding(true)} disabled={adding}>
          Add price
        </Button>
      </div>
      <p className="text-muted-foreground text-sm">
        Koinon prices every model request itself, from the tokens the provider reports and the price valid at that time. The prices you add win over the default list that ships with Koinon. Nothing you change here alters
        a request that was already charged.
      </p>
      {notice && (
        <p role="status" className="text-sm">
          {notice}
        </p>
      )}
      {warnings.length > 0 && (
        <Alert role="status" aria-label="Warnings">
          <AlertDescription>
            {warnings.map((w) => (
              <p key={w}>{w}</p>
            ))}
          </AlertDescription>
        </Alert>
      )}
      {refused && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{refused}</AlertDescription>
        </Alert>
      )}

      <section aria-labelledby="org-prices" className="grid gap-3">
        <h2 id="org-prices" className="text-lg font-semibold">
          Your prices
        </h2>
        {adding && <PriceForm orgId={orgId} actions={actions} onDone={() => setAdding(false)} onSaved={saved} />}
        {prices.error && (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{ConnectError.from(prices.error).rawMessage}</AlertDescription>
          </Alert>
        )}
        {prices.isPending ? (
          <p role="status">Loading prices…</p>
        ) : groups.length === 0 ? (
          <p className="text-muted-foreground text-sm">You have no prices of your own. Models use the default list below. Add one to override a default or to price a model the list does not know.</p>
        ) : (
          <ul className="grid gap-3" aria-label="Your prices">
            {groups.map((g) => (
              <li key={g.name} className="bg-card grid gap-2 rounded-md border p-3">
                <div className="flex flex-wrap items-center gap-2">
                  <h3 className="text-base font-semibold">{g.name}</h3>
                  {g.current?.needsReview && <Badge variant="destructive">Needs review</Badge>}
                  {g.current ? <Badge variant="secondary">Active</Badge> : <Badge variant="outline">{g.versions[0]?.retiredAt ? 'Retired' : 'Starts later'}</Badge>}
                </div>
                {g.current ? (
                  <>
                    {g.current.needsReview && (
                      <Alert role="note">
                        <AlertDescription>
                          This price came from the rates set on a model. No default price matched it, so its estimate settings (tokenizer, bytes per token, largest output) are generic. Check them and save a new version.
                        </AlertDescription>
                      </Alert>
                    )}
                    <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
                      <dt className="text-muted-foreground">Models</dt>
                      <dd className="break-all">
                        {g.current.provider === '*' ? 'any provider' : g.current.provider}: {matchLabel(g.current.match)}
                      </dd>
                      <dt className="text-muted-foreground">Prices</dt>
                      <dd>{ratesLine(g.current.rates)}</dd>
                      {g.current.tiers.map((t) => (
                        <TierRow key={String(t.contextOver)} over={String(t.contextOver)} line={ratesLine(t.rates)} />
                      ))}
                      <dt className="text-muted-foreground">Largest output</dt>
                      <dd>{g.current.maxOutput ? `${g.current.maxOutput.toLocaleString('en-US')} tokens` : 'not set (free)'}</dd>
                      <dt className="text-muted-foreground">Estimates</dt>
                      <dd>
                        {g.current.tokenizer}, {g.current.ratio} bytes per token
                      </dd>
                      <dt className="text-muted-foreground">Applies from</dt>
                      <dd>{formatWhen(g.current.validFrom)}</dd>
                    </dl>
                    {g.versions.length > 1 && <p className="text-muted-foreground text-xs">{g.versions.length} versions; earlier requests keep the version they were charged at.</p>}
                    {editing === g.name ? (
                      <PriceForm orgId={orgId} existing={g.current} actions={actions} onDone={closeOnly(setEditing, g.name)} onSaved={saved} />
                    ) : (
                      <div className="flex flex-wrap gap-2">
                        <Button variant="outline" size="sm" aria-label={`Edit price ${g.name}`} onClick={() => setEditing(g.name)}>
                          Edit
                        </Button>
                        <ConfirmAction
                          label="Retire"
                          subject={`price ${g.name}`}
                          consequence={`Retire ${g.name}? New requests use the next matching price, and past requests keep this one.`}
                          confirmLabel="Retire price"
                          onConfirm={async () => {
                            setRefused(undefined)
                            await actions.retirePrice(g.name)
                            setNotice(`Retired ${g.name}.`)
                          }}
                          onError={setRefused}
                        />
                      </div>
                    )}
                  </>
                ) : (
                  <RetiredVersions versions={g.versions} />
                )}
              </li>
            ))}
          </ul>
        )}
      </section>

      <DefaultPrices orgId={orgId} />
      <FlaggedPrices orgId={orgId} />
    </>
  )
}

function TierRow({ over, line }: { over: string; line: string }) {
  return (
    <>
      <dt className="text-muted-foreground">Above {Number(over).toLocaleString('en-US')} input tokens</dt>
      <dd>{line}</dd>
    </>
  )
}

function RetiredVersions({ versions }: { versions: PriceVersion[] }) {
  const v = versions[0]
  return (
    <p className="text-muted-foreground text-sm">
      {v?.retiredAt ? `Retired ${formatWhen(v.retiredAt)}.` : `Starts ${formatWhen(v?.validFrom)}.`} Add a price with the same name to bring it back.
    </p>
  )
}

function DefaultPrices({ orgId }: { orgId: string }) {
  const defaults = useQuery(PriceService.method.getDefaultPrices, { orgId })
  const changes = useQuery(PriceService.method.getDefaultPriceChanges, { orgId })
  const marks = useMemo(() => changeById(changes.data?.changes ?? []), [changes.data])
  const removed = (changes.data?.changes ?? []).filter((c) => c.kind === 'removed')
  const failed = defaults.error
  return (
    <section aria-labelledby="default-prices" className="grid gap-3">
      <h2 id="default-prices" className="text-lg font-semibold">
        Default prices
      </h2>
      <p className="text-muted-foreground text-sm">
        These come with Koinon, read only. Each links to the provider’s own pricing page. To change one for your org, add a price of your own above.
        {defaults.data && <> List version {defaults.data.version}.</>}
      </p>
      {failed && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(failed).rawMessage}</AlertDescription>
        </Alert>
      )}
      {changes.data && changes.data.fromVersion && (
        <p className="text-sm" role="status">
          {marks.size + removed.length === 0
            ? `The default prices did not change between ${changes.data.fromVersion} and ${changes.data.toVersion}.`
            : `${marks.size + removed.length} default ${marks.size + removed.length === 1 ? 'price' : 'prices'} changed since ${changes.data.fromVersion}${removed.length ? `; removed: ${removed.map((c) => c.id).join(', ')}` : ''}.`}
        </p>
      )}
      {defaults.isPending ? (
        <p role="status">Loading default prices…</p>
      ) : (
        <ul className="grid gap-3" aria-label="Default prices">
          {(defaults.data?.entries ?? []).map((e) => (
            <DefaultRow key={`${e.id}@${e.validFrom}`} entry={e} mark={marks.get(e.id)} />
          ))}
        </ul>
      )}
    </section>
  )
}

function DefaultRow({ entry: e, mark }: { entry: PriceEntry; mark?: Parameters<typeof changeLabel>[0] }) {
  const href = safeSource(e)
  return (
    <li className="bg-card grid gap-2 rounded-md border p-3">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="text-base font-semibold break-all">{e.id}</h3>
        {mark && (
          <Badge variant="outline" className="h-auto max-w-full rounded-md py-1 text-left whitespace-normal">
            {changeLabel(mark)}
          </Badge>
        )}
      </div>
      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
        <dt className="text-muted-foreground">Models</dt>
        <dd className="break-all">
          {e.provider === '*' ? 'any provider' : e.provider}: {matchLabel(e.match)}
        </dd>
        <dt className="text-muted-foreground">Prices</dt>
        <dd>{ratesLine(e.rates)}</dd>
        {e.tiers.map((t) => (
          <TierRow key={String(t.contextOver)} over={String(t.contextOver)} line={ratesLine(t.rates)} />
        ))}
        <dt className="text-muted-foreground">Valid from</dt>
        <dd>{e.validFrom}</dd>
        <dt className="text-muted-foreground">Source</dt>
        <dd className="break-all">
          {href ? (
            <a href={href} target="_blank" rel="noreferrer noopener" className="underline">
              {href}
            </a>
          ) : (
            'none'
          )}
          {e.sourceAccessed && <span className="text-muted-foreground"> (read {e.sourceAccessed})</span>}
        </dd>
      </dl>
      {e.note && <p className="text-muted-foreground text-xs">{e.note}</p>}
    </li>
  )
}

function FlaggedPrices({ orgId }: { orgId: string }) {
  const [days, setDays] = useState<number>(7)
  const [reason, setReason] = useState<FlagReason | ''>('')
  const [model, setModel] = useState('')
  const [opened] = useState(() => Date.now())
  const flagged = useQuery(PriceService.method.listFlaggedPrices, { orgId, since: sinceTimestamp(days, opened) })
  const shown = filterFlagged(flagged.data?.flagged ?? [], { reason, model })
  return (
    <section aria-labelledby="flagged-prices" className="grid gap-3">
      <h2 id="flagged-prices" className="text-lg font-semibold">
        Flagged requests
      </h2>
      <p className="text-muted-foreground text-sm">Requests that Koinon priced with doubt, grouped by price. A flag does not change what was charged; it tells you which price to look at.</p>
      <div className="grid gap-3 sm:grid-cols-3">
        <div className="grid gap-1.5">
          <Label htmlFor="flag-since">Period</Label>
          <NativeSelect id="flag-since" value={String(days)} onChange={(e) => setDays(Number(e.target.value))}>
            {sinceChoices.map((c) => (
              <option key={c.days} value={c.days}>
                {c.label}
              </option>
            ))}
          </NativeSelect>
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor="flag-reason">Reason</Label>
          <NativeSelect id="flag-reason" value={reason} onChange={(e) => setReason(e.target.value as FlagReason | '')}>
            <option value="">All reasons</option>
            {flagReasons.map((r) => (
              <option key={r.value} value={r.value}>
                {r.label}
              </option>
            ))}
          </NativeSelect>
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor="flag-model">Model or price</Label>
          <Input id="flag-model" value={model} onChange={(e) => setModel(e.target.value)} autoComplete="off" />
        </div>
      </div>
      {flagged.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(flagged.error).rawMessage}</AlertDescription>
        </Alert>
      )}
      {flagged.isPending ? (
        <p role="status">Loading flagged requests…</p>
      ) : shown.length === 0 ? (
        <p className="text-muted-foreground text-sm" role="status">
          {(flagged.data?.flagged.length ?? 0) === 0 ? 'No flagged requests in this period.' : 'No flagged requests match these filters.'}
        </p>
      ) : (
        <ul className="grid gap-3" aria-label="Flagged requests">
          {shown.map((f) => (
            <li key={`${f.priceRef}|${f.model}`} className="bg-card grid gap-1 rounded-md border p-3 text-sm">
              <div className="flex flex-wrap items-center gap-2">
                <code className="font-medium break-all">{f.model}</code>
                <span className="text-muted-foreground">
                  {f.requests} {f.requests === 1n ? 'request' : 'requests'}, last {formatWhen(f.lastSeen)}
                </span>
              </div>
              <p className="text-muted-foreground break-all">Price: {f.priceRef || 'none (the gateway’s cost stood in)'}</p>
              <ul className="grid gap-1" aria-label={`Reasons for ${f.model}`}>
                {reasonsOf(f).map((r) => {
                  const info = flagReasons.find((x) => x.value === r)!
                  return (
                    <li key={r}>
                      <span className="font-medium">{info.label}.</span> <span className="text-muted-foreground">{info.help}</span>
                    </li>
                  )
                })}
              </ul>
              {reasonsOf(f).includes('gateway_diff') && f.gatewayCostUsd && (
                <p>
                  Koinon {formatUSD(f.costUsd)}, gateway {formatUSD(f.gatewayCostUsd)}.
                </p>
              )}
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}
