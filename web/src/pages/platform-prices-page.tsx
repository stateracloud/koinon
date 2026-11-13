// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { useState, type FormEvent } from 'react'
import { ConfirmAction } from '@/components/confirm-action'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import { AuthService } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { PriceService, type PlatformPriceVersion, type PriceProposal, type PriceProvenance } from '@/gen/koinon/budget/v1alpha1/price_pb'
import { usePlatformPriceActions } from '@/hooks/use-spend-actions'
import { actorLabel, classLabel, formatWhen, rateChangeLine, ratesLine, shortHash } from '@/lib/prices'

/**
 * PlatformPricesPage is where a platform admin reviews the price proposals, sees the platform list's
 * versions and where each price came from, rolls back, reads the fetch log and checks the sources
 * (ADR 0098). Others get a notice.
 */
export function PlatformPricesPage() {
  const who = useQuery(AuthService.method.whoAmI, {})
  return (
    <div className="mx-auto grid max-w-4xl grid-cols-[minmax(0,1fr)] gap-6">
      {who.isPending ? (
        <p role="status">Loading…</p>
      ) : who.data?.platformAdmin ? (
        <PlatformPrices />
      ) : (
        <Alert role="alert">
          <AlertDescription>Only platform admins can manage the platform prices.</AlertDescription>
        </Alert>
      )}
    </div>
  )
}

function PlatformPrices() {
  const actions = usePlatformPriceActions()
  const [notice, setNotice] = useState<string>()
  const [refused, setRefused] = useState<string>()
  const [checking, setChecking] = useState(false)
  const settings = useQuery(PriceService.method.getPriceRefreshSettings, {})
  const say = (m: string) => {
    setRefused(undefined)
    setNotice(m)
  }
  const refuse = (m: string) => {
    setNotice(undefined)
    setRefused(m)
  }
  async function check() {
    setChecking(true)
    try {
      const r = await actions.checkSources()
      const failed = Object.entries(r.failed)
      say(`Checked ${r.fetched.length} source(s); ${r.proposals} proposal(s) made or refreshed.${failed.map(([s, why]) => ` ${s} failed: ${why}.`).join('')}`)
    } catch (e) {
      refuse(ConnectError.from(e).rawMessage)
    } finally {
      setChecking(false)
    }
  }
  const s = settings.data
  return (
    <>
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold">Platform prices</h1>
        <Button size="sm" className="ml-auto" onClick={check} disabled={checking}>
          Check sources
        </Button>
      </div>
      <p className="text-muted-foreground text-sm">
        The platform list sits between an org’s own prices and the default list that ships with Koinon. A source only proposes: nothing changes until a person accepts it here, or the install’s bounded auto-apply does.
        Every version is kept; a rollback writes a new version.
      </p>
      {s && (
        <p className="text-muted-foreground text-sm" aria-label="Refresh setting">
          Scheduled refresh: {s.mode}
          {s.mode !== 'off' && `, every ${s.interval}`}; auto-apply up to {s.maxChangePercent}% and {s.maxAutoPerDay} a day. Set by the install’s Helm values.
          {!s.recorded && ' (Defaults: koinon-api has not recorded a setting yet.)'}
        </p>
      )}
      {notice && (
        <p role="status" className="text-sm">
          {notice}
        </p>
      )}
      {refused && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{refused}</AlertDescription>
        </Alert>
      )}
      <Proposals say={say} refuse={refuse} actions={actions} />
      <Versions say={say} refuse={refuse} actions={actions} />
      <Explain />
      <FetchLog />
    </>
  )
}

type Actions = ReturnType<typeof usePlatformPriceActions>
type Feedback = { say: (m: string) => void; refuse: (m: string) => void; actions: Actions }

function Provenance({ p }: { p: PriceProvenance | undefined }) {
  if (!p) return null
  return (
    <dl className="text-muted-foreground grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-0.5 text-xs">
      <dt>Source</dt>
      <dd>
        {p.source}
        {p.url && <span className="break-all"> · {p.url}</span>}
      </dd>
      {p.fetchedAt && (
        <>
          <dt>Fetched</dt>
          <dd>{formatWhen(p.fetchedAt)}</dd>
        </>
      )}
      {p.sha256 && (
        <>
          <dt>Body sha256</dt>
          <dd className="font-mono break-all">{p.sha256}</dd>
        </>
      )}
      {p.commitSha && (
        <>
          <dt>Commit</dt>
          <dd className="font-mono">{shortHash(p.commitSha)}</dd>
        </>
      )}
      {p.etag && (
        <>
          <dt>ETag</dt>
          <dd className="font-mono break-all">{p.etag}</dd>
        </>
      )}
      {p.agreeingSources.length > 0 && (
        <>
          <dt>Agreeing sources</dt>
          <dd>{p.agreeingSources.join(', ')}</dd>
        </>
      )}
      {p.approver && (
        <>
          <dt>Approved by</dt>
          <dd>
            {actorLabel(p.approver)}
            {p.ruleSetVersion && ` · rule set ${p.ruleSetVersion}`}
          </dd>
        </>
      )}
      {p.vouchedBy && (
        <>
          <dt>Imported by</dt>
          <dd>{actorLabel(p.vouchedBy)} (the body hash could not be re-checked)</dd>
        </>
      )}
    </dl>
  )
}

function Proposals({ say, refuse, actions }: Feedback) {
  const [klass, setKlass] = useState('')
  const list = useQuery(PriceService.method.listPriceProposals, { state: 'open', class: klass })
  return (
    <section aria-labelledby="proposals" className="grid gap-3">
      <div className="flex flex-wrap items-center gap-3">
        <h2 id="proposals" className="text-lg font-semibold">
          Proposals
        </h2>
        <div className="ml-auto flex items-center gap-2 text-sm">
          <Label htmlFor="proposal-class">Class</Label>
          <NativeSelect id="proposal-class" value={klass} onChange={(e) => setKlass(e.target.value)}>
            <option value="">All open</option>
            <option value="confirm">Confirm</option>
            <option value="needs_review">Needs review</option>
            <option value="auto">Auto</option>
            <option value="refused">Refused</option>
          </NativeSelect>
        </div>
      </div>
      {list.isPending ? (
        <p role="status">Loading…</p>
      ) : list.error ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(list.error).rawMessage}</AlertDescription>
        </Alert>
      ) : list.data.proposals.length === 0 ? (
        <p className="text-muted-foreground text-sm">No open proposals.</p>
      ) : (
        <ul className="grid gap-3">
          {list.data.proposals.map((p) => (
            <ProposalCard key={p.id} p={p} say={say} refuse={refuse} actions={actions} />
          ))}
        </ul>
      )}
    </section>
  )
}

function ProposalCard({ p, say, refuse, actions }: { p: PriceProposal } & Feedback) {
  const [typed, setTyped] = useState('')
  const [maxOutput, setMaxOutput] = useState('')
  const [busy, setBusy] = useState(false)
  const needsConfirm = p.class === 'confirm'
  const needsMax = !!p.proposed && p.proposed.maxOutput === 0
  const refusedClass = p.class === 'refused'
  const maxN = Number(maxOutput)
  const maxBad = needsMax && (!Number.isInteger(maxN) || maxN < 1)
  async function accept(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    try {
      const r = await actions.accept(p.id, typed, needsMax ? maxN : 0)
      say(`Accepted ${p.model}: platform price version ${r.version} written.${r.warnings.map((w) => ` ${w}`).join('')}`)
    } catch (err) {
      refuse(ConnectError.from(err).rawMessage)
    } finally {
      setBusy(false)
    }
  }
  return (
    <li className="grid gap-2 rounded-lg border p-3">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="font-medium break-all">{p.model}</h3>
        <Badge variant={refusedClass || needsConfirm ? 'destructive' : 'secondary'}>{classLabel(p.class)}</Badge>
        <span className="text-muted-foreground text-xs">
          {p.providerType} · from {p.source}
          {p.origin === 'import' && ' (imported file)'} · {formatWhen(p.createdAt)}
        </span>
      </div>
      {p.failedRules && <p className="text-sm">{p.failedRules}</p>}
      <div className="grid gap-1 text-sm sm:grid-cols-2">
        <div>
          <p className="text-muted-foreground text-xs">In use{p.currentRef ? ` (${p.currentRef})` : ''}</p>
          <p>{p.current ? ratesLine(p.current.rates) : 'No price: a new model'}</p>
        </div>
        <div>
          <p className="text-muted-foreground text-xs">Proposed</p>
          <p>{p.proposed ? ratesLine(p.proposed.rates) : 'No price'}</p>
        </div>
      </div>
      {p.changes.length > 0 && (
        <ul aria-label={`Changes for ${p.model}`} className="list-inside list-disc text-sm">
          {p.changes.map((c) => (
            <li key={`${c.bucket}-${c.contextOver}`}>{rateChangeLine(c)}</li>
          ))}
        </ul>
      )}
      <Provenance p={p.provenance} />
      {!refusedClass && (
        <form onSubmit={accept} className="flex flex-wrap items-end gap-3" aria-label={`Decide ${p.model}`}>
          {needsConfirm && (
            <div className="grid gap-1">
              <Label htmlFor={`confirm-${p.id}`}>Type the model id to confirm</Label>
              <Input id={`confirm-${p.id}`} value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" placeholder={p.model} />
            </div>
          )}
          {needsMax && (
            <div className="grid gap-1">
              <Label htmlFor={`max-${p.id}`}>Output limit in tokens</Label>
              <Input id={`max-${p.id}`} inputMode="numeric" value={maxOutput} onChange={(e) => setMaxOutput(e.target.value)} placeholder="from the provider’s model page" />
            </div>
          )}
          <Button type="submit" size="sm" disabled={busy || (needsConfirm && typed !== p.model) || maxBad} aria-label={`Accept ${p.model}`}>
            Accept
          </Button>
        </form>
      )}
      <div>
        <ConfirmAction
          label="Dismiss"
          subject={p.model}
          consequence="The same values from the same source will not be proposed again."
          confirmLabel="Dismiss proposal"
          onConfirm={async () => {
            await actions.dismiss(p.id)
            say(`Dismissed the proposal for ${p.model}.`)
          }}
          onError={refuse}
        />
      </div>
    </li>
  )
}

function Versions({ say, refuse, actions }: Feedback) {
  const versions = useQuery(PriceService.method.listPlatformPriceVersions, { limit: 50 })
  const [open, setOpen] = useState<bigint>()
  return (
    <section aria-labelledby="versions" className="grid gap-3">
      <h2 id="versions" className="text-lg font-semibold">
        Versions
      </h2>
      {versions.isPending ? (
        <p role="status">Loading…</p>
      ) : versions.error ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(versions.error).rawMessage}</AlertDescription>
        </Alert>
      ) : versions.data.versions.length === 0 ? (
        <p className="text-muted-foreground text-sm">No platform version yet. Models are priced from the default list.</p>
      ) : (
        <ul className="grid gap-2">
          {versions.data.versions.map((v, i) => (
            <VersionRow key={v.version} v={v} latest={i === 0} open={open === v.version} onToggle={() => setOpen(open === v.version ? undefined : v.version)} say={say} refuse={refuse} actions={actions} />
          ))}
        </ul>
      )}
    </section>
  )
}

function VersionRow({ v, latest, open, onToggle, say, refuse, actions }: { v: PlatformPriceVersion; latest: boolean; open: boolean; onToggle: () => void } & Feedback) {
  return (
    <li className="grid gap-2 rounded-lg border p-3">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="font-medium">Version {String(v.version)}</h3>
        {latest && <Badge variant="secondary">Newest</Badge>}
        <span className="text-muted-foreground text-xs">
          {formatWhen(v.createdAt)} · {actorLabel(v.actor)}
          {v.restoredFrom > 0n && ` · restores version ${String(v.restoredFrom)}`}
        </span>
        <div className="ml-auto flex items-center gap-2">
          <Button variant="outline" size="sm" onClick={onToggle} aria-expanded={open} aria-label={`${open ? 'Hide' : 'Show'} changes in version ${String(v.version)}`}>
            {open ? 'Hide changes' : 'Show changes'}
          </Button>
          {!latest && (
            <ConfirmAction
              label="Roll back"
              subject={`to version ${String(v.version)}`}
              consequence={`Writes a new version equal to version ${String(v.version)}, from now on. Requests already charged keep their price.`}
              confirmLabel="Roll back"
              onConfirm={async () => {
                const r = await actions.rollback(v.version)
                say(`Platform price version ${String(r.version)} written, equal to version ${String(v.version)}.`)
              }}
              onError={refuse}
            />
          )}
        </div>
      </div>
      <p className="text-sm">{v.reason}</p>
      {open && <VersionDetail version={v.version} />}
    </li>
  )
}

function VersionDetail({ version }: { version: bigint }) {
  const diff = useQuery(PriceService.method.diffPlatformPriceVersions, { toVersion: version })
  const detail = useQuery(PriceService.method.getPlatformPriceVersion, { version })
  return (
    <div className="grid gap-3 border-t pt-2">
      <div>
        <h4 className="text-sm font-medium">Changes from the version before</h4>
        {diff.isPending ? (
          <p role="status">Loading…</p>
        ) : diff.error ? (
          <p role="alert">{ConnectError.from(diff.error).rawMessage}</p>
        ) : diff.data.changes.length === 0 ? (
          <p className="text-muted-foreground text-sm">No change.</p>
        ) : (
          <ul className="text-sm">
            {diff.data.changes.map((c) => (
              <li key={`${c.id}-${c.validFrom}`}>
                <span className="font-medium">{c.kind}</span> {c.id}
                {c.before && c.after && ` · ${ratesLine(c.before.rates)} → ${ratesLine(c.after.rates)}`}
                {!c.before && c.after && ` · ${ratesLine(c.after.rates)}`}
                {c.fields.length > 0 && <span className="text-muted-foreground"> ({c.fields.join(', ')})</span>}
              </li>
            ))}
          </ul>
        )}
      </div>
      <div>
        <h4 className="text-sm font-medium">Prices in this version</h4>
        {detail.isPending ? (
          <p role="status">Loading…</p>
        ) : detail.error ? (
          <p role="alert">{ConnectError.from(detail.error).rawMessage}</p>
        ) : (
          <ul className="grid gap-2">
            {(detail.data.version?.entries ?? []).map((e) => (
              <li key={e.priceRef} className="grid gap-1 text-sm">
                <p>
                  <span className="font-medium break-all">{e.entry?.match?.glob || e.entry?.match?.regex}</span> <span className="text-muted-foreground font-mono text-xs">{e.priceRef}</span>
                </p>
                <p>{ratesLine(e.entry?.rates)}</p>
                <Provenance p={e.provenance} />
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  )
}

function layerLabel(layer: string): string {
  return layer === 'platform' ? 'Platform list' : layer === 'default' ? 'Default list' : layer
}

function Explain() {
  const [providerType, setProviderType] = useState('openrouter')
  const [model, setModel] = useState('')
  const [ask, setAsk] = useState<{ providerType: string; model: string }>()
  const res = useQuery(PriceService.method.explainPlatformPrice, ask ?? { providerType: '', model: '' }, { enabled: !!ask })
  return (
    <section aria-labelledby="explain" className="grid gap-3">
      <h2 id="explain" className="text-lg font-semibold">
        Which price applies
      </h2>
      <form
        className="flex flex-wrap items-end gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          setAsk({ providerType, model: model.trim() })
        }}
      >
        <div className="grid gap-1">
          <Label htmlFor="explain-provider">Provider type</Label>
          <Input id="explain-provider" value={providerType} onChange={(e) => setProviderType(e.target.value)} />
        </div>
        <div className="grid gap-1">
          <Label htmlFor="explain-model">Model</Label>
          <Input id="explain-model" value={model} onChange={(e) => setModel(e.target.value)} />
        </div>
        <Button type="submit" size="sm" disabled={!model.trim() || !providerType.trim()}>
          Show price
        </Button>
      </form>
      {ask && res.isPending && <p role="status">Loading…</p>}
      {ask && res.error && <p role="alert">{ConnectError.from(res.error).rawMessage}</p>}
      {ask && res.data && (
        <div aria-label="Price in use" className="grid gap-1 text-sm">
          <p>
            <Badge variant="secondary">{layerLabel(res.data.layer)}</Badge> <span className="font-mono text-xs">{res.data.priceRef}</span>
          </p>
          <p>{ratesLine(res.data.entry?.rates)}</p>
          <p className="text-muted-foreground text-xs">An org’s own price for the model still wins over this.</p>
          <Provenance p={res.data.provenance} />
        </div>
      )}
    </section>
  )
}

function FetchLog() {
  const fetches = useQuery(PriceService.method.listPriceSourceFetches, { limit: 50 })
  return (
    <section aria-labelledby="fetch-log" className="grid gap-3">
      <h2 id="fetch-log" className="text-lg font-semibold">
        Fetch log
      </h2>
      {fetches.isPending ? (
        <p role="status">Loading…</p>
      ) : fetches.error ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(fetches.error).rawMessage}</AlertDescription>
        </Alert>
      ) : fetches.data.fetches.length === 0 ? (
        <p className="text-muted-foreground text-sm">No source has been fetched yet.</p>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full text-left text-sm">
            <caption className="sr-only">Price source fetches, newest first</caption>
            <thead>
              <tr>
                <th scope="col">When</th>
                <th scope="col">Source</th>
                <th scope="col">Result</th>
                <th scope="col">Models</th>
                <th scope="col">Trigger</th>
                <th scope="col">Detail</th>
              </tr>
            </thead>
            <tbody>
              {fetches.data.fetches.map((f) => (
                <tr key={f.id} className="border-t align-top">
                  <td>{formatWhen(f.fetchedAt)}</td>
                  <td>{f.source}</td>
                  <td>{f.result}</td>
                  <td>{f.modelCount}</td>
                  <td>{f.trigger}</td>
                  <td className="break-all">{f.error || (f.sha256 ? `sha256 ${shortHash(f.sha256)}` : '')}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
