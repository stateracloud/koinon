// SPDX-License-Identifier: Apache-2.0

import { useState } from 'react'
import { marketItems } from '@/preview/mock/data'
import { AgentAvatar, Badge, Button, Input, Label, PreviewLink, PreviewShell, usePathname } from '@/preview/ui'
import { scanFindings, skillFiles, skillUsage, skillVersions } from './data'

/**
 * SkillDetailScreen is Skill-detail.dc.html: the review gate, scan findings, the diff to the version in use, the files,
 * versions and where the skill is used. Approve v5 flips the status; Block… opens the reason panel. Every skill id
 * shows helm-upgrade's sample data, since the mockup has none for the others; only the name and description change.
 */
export function SkillDetailScreen() {
  const path = usePathname()
  const id = decodeURIComponent(path.split('/').pop() ?? 'helm-upgrade')
  const item = marketItems.find((m) => m.skillId === id)
  const name = item?.name ?? 'helm-upgrade'
  const description = item?.description ?? 'Render a chart diff against running values and explain CRD changes.'
  const [approved, setApproved] = useState(false)
  const [blocking, setBlocking] = useState(false)
  const status = approved ? 'Approved' : 'Needs review'
  const statusLine = approved
    ? 'Approved by procoduck just now, with scans 7f3e and 7f3f.'
    : 'Needs review: the medium finding meets the org threshold, so a person decides. Agents keep v4 until then.'

  return (
    <PreviewShell space="org" current="Skills" crumbs={[{ label: 'Skills', href: '/preview/marketplace' }, { label: name, mono: true }]}>
      <div className="ks-head">
        <div className="ks-stack" style={{ gap: 4 }}>
          <div className="ks-row">
            <h1 className="ks-h1 ks-mono" style={{ letterSpacing: 0 }}>
              {name}
            </h1>
            <Badge variant="secondary">Org skill</Badge>
            <Badge variant="outline">{status}</Badge>
            <Badge variant="outline">Update available</Badge>
          </div>
          <p className="ks-p ks-muted">
            {description.replace(/\.$/, '')}. From <span className="ks-mono">stateracloud/platform-skills</span>, path{' '}
            <span className="ks-mono">skills/{name}</span>, ref <span className="ks-mono">main</span>.
          </p>
        </div>
        <div className="ks-row">
          <Button variant="outline" size="sm">
            Check now
          </Button>
          <Button variant="outline" size="sm" onClick={() => setBlocking(true)} aria-expanded={blocking} aria-controls="skill-block">
            Block…
          </Button>
          <Button size="sm" onClick={() => setApproved(true)} disabled={approved}>
            Approve v5
          </Button>
        </div>
      </div>

      {blocking && (
        <div id="skill-block" className="ks-panel ks-stack-3" style={{ borderColor: 'var(--danger-border)' }}>
          <Label htmlFor="blk">Reason for blocking v5 in Platform Engineering</Label>
          <Input id="blk" defaultValue="Script curls an unpinned URL; wait for upstream fix." />
          <p className="ks-p ks-meta">Blocking stops at once every run whose agent version uses v5. It is audited.</p>
          <div className="ks-row" style={{ justifyContent: 'flex-end' }}>
            <Button variant="outline" onClick={() => setBlocking(false)}>
              Cancel
            </Button>
            <Button variant="destructive">Block v5</Button>
          </div>
        </div>
      )}

      <div className="ks-split">
        <div className="ks-main-part ks-stack-4">
          <section className="ks-panel ks-stack-3" aria-labelledby="gate-h">
            <div className="ks-between">
              <h2 id="gate-h" className="ks-h4">
                Review gate
              </h2>
              <PreviewLink className="ks-link" href="/preview/platform/settings">
                Gate settings
              </PreviewLink>
            </div>
            <dl className="ks-kv">
              <dt>Gate</dt>
              <dd>
                <span className="ks-mono">auto-if-clean</span>, threshold medium <Badge variant="outline">Org override</Badge> <span className="ks-meta">Platform default: auto</span>
              </dd>
              <dt>v5 status</dt>
              <dd>{statusLine}</dd>
              <dt>Judge</dt>
              <dd>Off. Static scanners alone catch few determined attacks; turn the judge on for sensitive orgs.</dd>
            </dl>
          </section>

          <section className="ks-panel ks-stack-3" aria-labelledby="find-h">
            <div className="ks-between">
              <h2 id="find-h" className="ks-h4">
                Scan findings, v5
              </h2>
              <span className="ks-meta">SkillSpector 0.9.2 (no LLM) · Cisco skill-scanner 1.4.0 static · rules 2026-10-20</span>
            </div>
            <div className="ks-table-wrap">
              <table className="ks-table">
                <thead>
                  <tr>
                    <th scope="col">Severity</th>
                    <th scope="col">Scanner</th>
                    <th scope="col">Rule</th>
                    <th scope="col">Where</th>
                    <th scope="col">Message</th>
                  </tr>
                </thead>
                <tbody>
                  {scanFindings.map((f) => (
                    <tr key={f.rule}>
                      <td>
                        <Badge variant={f.tone}>{f.severity}</Badge>
                      </td>
                      <td>{f.scanner}</td>
                      <td className="ks-mono">{f.rule}</td>
                      <td className="ks-mono">{f.where}</td>
                      <td>{f.message}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </section>

          <section className="ks-panel ks-stack-3" aria-labelledby="diff-h">
            <div className="ks-between">
              <h2 id="diff-h" className="ks-h4">
                Diff, v4 (in use) to v5
              </h2>
              <span className="ks-meta">2 files changed</span>
            </div>
            <div className="ks-diff">
              <div className="ks-muted">scripts/diff.py</div>
              <div className="ks-del">{'-    out = subprocess.run(["helm", "diff", "upgrade", rel, chart], capture_output=True)'}</div>
              <div className="ks-add">{'+    out = subprocess.run(f"helm diff upgrade {rel} {chart} --three-way-merge", shell=True, capture_output=True)'}</div>
              <div className="ks-muted">SKILL.md</div>
              <div className="ks-add">+ Use --three-way-merge so manual edits in the cluster show up in the diff.</div>
            </div>
          </section>

          <section className="ks-panel ks-stack-3" aria-labelledby="files-h">
            <h2 id="files-h" className="ks-h4">
              Files, v5
            </h2>
            <div className="ks-files">
              {skillFiles.map((f) => (
                <div key={f.name} className="ks-file" style={{ paddingLeft: f.pad, cursor: 'default' }}>
                  <span className="ks-grow">{f.name}</span>
                  <span className="ks-meta">{f.meta}</span>
                </div>
              ))}
            </div>
          </section>
        </div>

        <aside className="ks-aside ks-stack-4">
          <section className="ks-panel ks-stack-3" aria-labelledby="ver-h">
            <h2 id="ver-h" className="ks-h4">
              Versions
            </h2>
            <ul className="ks-ul ks-stack">
              {skillVersions.map((v) => (
                <li key={v.label} className="ks-raised ks-stack" style={{ padding: '10px 12px', gap: 2 }}>
                  <span className="ks-between">
                    <span className="ks-mono ks-strong">{v.label}</span>
                    <span className="ks-meta">{v.status ?? status}</span>
                  </span>
                  <span className="ks-meta">{v.note}</span>
                </li>
              ))}
            </ul>
          </section>
          <section className="ks-panel ks-stack-3" aria-labelledby="used-h">
            <h2 id="used-h" className="ks-h4">
              Where it is used
            </h2>
            <ul className="ks-ul ks-stack" style={{ fontSize: 13 }}>
              {skillUsage.map((u) => (
                <li key={u.name} className="ks-between">
                  {'plain' in u ? (
                    <span>{u.name}</span>
                  ) : 'href' in u ? (
                    <PreviewLink className="ks-row ks-a" href={u.href} style={{ gap: 6 }}>
                      <AgentAvatar name={u.name} decorative />
                      {u.name}
                    </PreviewLink>
                  ) : (
                    <span className="ks-row" style={{ gap: 6 }}>
                      <AgentAvatar name={u.name} decorative />
                      {u.name}
                    </span>
                  )}
                  <span className="ks-meta">{u.note}</span>
                </li>
              ))}
            </ul>
          </section>
        </aside>
      </div>
    </PreviewShell>
  )
}
