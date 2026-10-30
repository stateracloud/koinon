// SPDX-License-Identifier: Apache-2.0

import { useRef, useState } from 'react'
import { Alert, AlertDescription, AlertTitle, Button, NativeSelect, PageHeader, Panel, PreviewShell, StateDot, Table, Tabs, Textarea } from '@/preview/ui'
import { editorFiles, importCandidates, pasteSample, skillMdErrorLine, skillMdLines } from './data'

type Tab = 'line' | 'upload' | 'paste' | 'editor'

function OneLine() {
  const [picked, setPicked] = useState(() => new Set(importCandidates.filter((c) => c.picked).map((c) => c.id)))
  const [imported, setImported] = useState(false)
  const n = picked.size
  function toggle(id: string) {
    const next = new Set(picked)
    if (!next.delete(id)) next.add(id)
    setPicked(next)
    setImported(false)
  }
  return (
    <div className="ks-stack-4">
      <div className="ks-stack">
        <label className="kn-label" htmlFor="src">
          Paste a command, <span className="ks-mono">owner/repo</span>, a git URL or a marketplace
        </label>
        <div className="ks-row" style={{ flexWrap: 'nowrap' }}>
          <input id="src" className="kn-field kn-input ks-mono" type="text" defaultValue="npx skills add anthropics/skills --skill pdf --skill xlsx -g" />
          <Button variant="outline">Resolve</Button>
        </div>
        <span className="ks-meta">
          Also works: <span className="ks-mono">owner/repo@skill</span>, <span className="ks-mono">#ref</span>, <span className="ks-mono">/plugin marketplace add …</span>, <span className="ks-mono">plugin@marketplace</span>, a SKILL.md or archive URL. Same as <span className="ks-mono">koinon skills add anthropics/skills --skill pdf</span>.
        </span>
      </div>
      <Panel className="ks-stack-3">
        <div className="ks-between">
          <span>
            <span style={{ fontWeight: 600 }}>anthropics/skills</span>{' '}
            <span className="ks-meta">
              · main → commit <span className="ks-mono">4f2a9c1</span> · 17 skills found
            </span>
          </span>
          <span className="ks-meta">
            Ignored: <span className="ks-mono">-g</span> (Koinon decides where skills go)
          </span>
        </div>
        <Table label="Skills found">
          <thead>
            <tr>
              <th scope="col">
                <span className="ks-sr">Pick</span>
              </th>
              <th scope="col">Skill</th>
              <th scope="col">Path</th>
              <th scope="col">Licence</th>
              <th scope="col" className="ks-r">
                Files
              </th>
              <th scope="col" className="ks-r">
                Size
              </th>
            </tr>
          </thead>
          <tbody>
            {importCandidates.map((c) => (
              <tr key={c.id} aria-selected={picked.has(c.id) || undefined}>
                <td>
                  <input type="checkbox" checked={picked.has(c.id)} onChange={() => toggle(c.id)} aria-label={`Pick ${c.id}`} />
                </td>
                <td>
                  <span className="ks-mono">{c.id}</span>
                  <div className="ks-meta">{c.description}</div>
                  {c.needs && <span className="ks-warn ks-meta">Needs packages: {c.needs}</span>}
                </td>
                <td className="ks-mono">{c.path}</td>
                <td>{c.licence}</td>
                <td className="ks-r">{c.files}</td>
                <td className="ks-r">{c.size}</td>
              </tr>
            ))}
          </tbody>
        </Table>
        <div className="ks-row" style={{ justifyContent: 'flex-end' }}>
          <span className="ks-meta" style={{ marginRight: 'auto' }}>
            {n} of 17 picked
          </span>
          <Button disabled={n === 0} onClick={() => setImported(true)}>
            Import {n} {n === 1 ? 'skill' : 'skills'}
          </Button>
        </div>
        {imported && (
          <div className="ks-raised ks-stack" role="status" style={{ padding: 12 }}>
            {importCandidates
              .filter((c) => picked.has(c.id))
              .map((c, i) => (
                <span key={c.id} className="ks-row">
                  <StateDot state={i === 0 ? 'working' : 'completed'} />
                  <span className="ks-mono">{c.id}</span>
                  {i === 0 ? ' scanning with SkillSpector and Cisco skill-scanner…' : ' scanned · 0 findings · Approved by gate:auto'}
                </span>
              ))}
          </div>
        )}
      </Panel>
    </div>
  )
}

function Upload() {
  const input = useRef<HTMLInputElement>(null)
  const [names, setNames] = useState<string[]>([])
  const [over, setOver] = useState(false)
  return (
    <Panel
      className="ks-stack-3"
      style={{ borderStyle: 'dashed', padding: 40, textAlign: 'center', justifyItems: 'center', ...(over ? { borderColor: 'var(--primary)' } : null) }}
      onDragOver={(e) => {
        e.preventDefault()
        setOver(true)
      }}
      onDragLeave={() => setOver(false)}
      onDrop={(e) => {
        e.preventDefault()
        setOver(false)
        setNames(Array.from(e.dataTransfer.files).map((f) => f.name))
      }}
    >
      <span className="ks-tile-icon" aria-hidden="true">
        <svg className="ks-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
          <path d="m17 8-5-5-5 5" />
          <path d="M12 3v12" />
        </svg>
      </span>
      <span className="ks-h4">Drop a skill folder or a .zip or .tar archive</span>
      <span className="ks-muted">Every file keeps its path and executable bit. Up to 500 files, 16 MiB, six folders deep.</span>
      <input ref={input} type="file" multiple className="ks-sr" tabIndex={-1} aria-hidden="true" onChange={(e) => setNames(Array.from(e.target.files ?? []).map((f) => f.name))} />
      <Button variant="outline" onClick={() => input.current?.click()}>
        Choose files
      </Button>
      {names.length > 0 && (
        <span className="ks-meta ks-mono" role="status">
          {names.length} {names.length === 1 ? 'file' : 'files'} chosen: {names.slice(0, 3).join(', ')}
          {names.length > 3 ? '…' : ''}
        </span>
      )}
    </Panel>
  )
}

function Paste({ toEditor }: { toEditor: () => void }) {
  const [text, setText] = useState(pasteSample)
  const [saved, setSaved] = useState(false)
  const name = /^name:\s*(.+)$/m.exec(text)?.[1]?.trim() ?? ''
  return (
    <div className="ks-stack-3">
      <label className="kn-label" htmlFor="paste">
        Paste a SKILL.md
      </label>
      <Textarea id="paste" className="ks-mono" style={{ minHeight: 220 }} value={text} onChange={(e) => (setText(e.target.value), setSaved(false))} />
      <span className="ks-meta">
        The skill's folder takes its name from <span className="ks-mono">name</span>: {name || '…'}/
      </span>
      <div className="ks-row" style={{ justifyContent: 'flex-end' }}>
        {saved && (
          <span className="ks-meta" role="status" style={{ marginRight: 'auto' }}>
            Saved {name}/ as version 1. Scanning…
          </span>
        )}
        <Button variant="outline" onClick={toEditor}>
          Add files in the editor
        </Button>
        <Button disabled={!name} onClick={() => setSaved(true)}>
          Save version
        </Button>
      </div>
    </div>
  )
}

function Editor() {
  const [file, setFile] = useState('SKILL.md')
  const [saved, setSaved] = useState(false)
  const isSkill = file === 'SKILL.md'
  const folder = /\/$/.test(file)
  const lines = isSkill ? skillMdLines : folder ? [] : [`# ${file}`, '', 'Sample content of this file is not part of the mockup.']
  return (
    <div className="ks-split">
      <div className="ks-aside ks-panel ks-stack" style={{ flex: '0 1 280px' }}>
        <div className="ks-between">
          <span className="ks-mono ks-strong">k8s-hygiene/</span>
          <span className="ks-row" style={{ gap: 4 }}>
            <Button variant="ghost" size="xs">
              Add file
            </Button>
            <Button variant="ghost" size="xs">
              Add folder
            </Button>
          </span>
        </div>
        <div className="ks-files" role="tree" aria-label="Files">
          {editorFiles.map((f) => (
            <button key={f.name} type="button" className="ks-file" role="treeitem" aria-selected={f.name === file} aria-current={f.name === file ? 'true' : undefined} onClick={() => setFile(f.name)} style={{ paddingLeft: f.pad }}>
              <span className="ks-grow">{f.name}</span>
              <span className="ks-meta">{f.meta}</span>
              {f.error && (
                <span className="ks-err" aria-label="Has an error">
                  1
                </span>
              )}
            </button>
          ))}
        </div>
        <div className="ks-divider" />
        <span className="ks-meta ks-num">6 files · 18 KB · digest covers the whole tree</span>
      </div>
      <div className="ks-main-part ks-stack-3">
        <div className="ks-between">
          <span className="ks-mono">k8s-hygiene/{file}</span>
          <span className="ks-row">
            <span className="ks-meta">Body 1,240 tokens (limit 5,000)</span>
            <Button size="sm" onClick={() => setSaved(true)}>
              {saved ? 'Saved as version 1' : 'Save as version 1'}
            </Button>
          </span>
        </div>
        <div className="ks-log" aria-label="Editor">
          {lines.length === 0 && <div className="ks-line" style={{ gridTemplateColumns: '1fr' }}><span className="ks-msg ks-muted">A folder. Pick a file to edit.</span></div>}
          {lines.map((t, i) => (
            <div key={i} className="ks-line" data-lv={isSkill && i === skillMdErrorLine ? 'err' : ''} style={{ gridTemplateColumns: '36px 1fr' }}>
              <a href="#" onClick={(e) => e.preventDefault()}>
                {i + 1}
              </a>
              <span className="ks-msg">{t || ' '}</span>
            </div>
          ))}
        </div>
        <Alert variant="destructive">
          <AlertTitle>SKILL.md, line 14: link does not resolve</AlertTitle>
          <AlertDescription>references/limits.md is not in the skill. Add the file, or fix the link.</AlertDescription>
        </Alert>
      </div>
    </div>
  )
}

export function SkillAddScreen() {
  const [tab, setTab] = useState<Tab>('line')
  return (
    <PreviewShell space="org" current="Skills" crumbs={[{ label: 'Marketplace', href: '/preview/marketplace' }, { label: 'Add a skill' }]}>
      <PageHeader
        title="Add a skill"
        description="Koinon fetches it, pins the commit and the content digest, scans it and applies the org's review gate (auto). Nothing is installed in a sandbox."
        actions={
          <div className="ks-stack" style={{ gap: 4 }}>
            <label className="kn-label" htmlFor="lib">
              Library
            </label>
            <NativeSelect id="lib" style={{ width: 240 }} defaultValue="org">
              <option value="org">Platform Engineering (org)</option>
              <option value="platform">Platform (platform admins)</option>
            </NativeSelect>
          </div>
        }
      />
      <Tabs
        label="How to add"
        idPrefix="sa"
        value={tab}
        onChange={setTab}
        tabs={[
          { id: 'line', label: 'One line' },
          { id: 'upload', label: 'Upload' },
          { id: 'paste', label: 'Paste SKILL.md' },
          { id: 'editor', label: 'New skill (editor)' },
        ]}
      />
      <div role="tabpanel" id={`sa-${tab}-panel`} aria-labelledby={`sa-${tab}-tab`}>
        {tab === 'line' && <OneLine />}
        {tab === 'upload' && <Upload />}
        {tab === 'paste' && <Paste toEditor={() => setTab('editor')} />}
        {tab === 'editor' && <Editor />}
      </div>
    </PreviewShell>
  )
}

