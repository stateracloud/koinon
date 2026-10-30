// SPDX-License-Identifier: Apache-2.0

// Screen data of the onboarding group, seeded verbatim from FirstLogin, Idion-firstrun and Idion-home.

export type OnboardingPath = 'join' | 'create' | 'ask'
export type OnboardingStep = 'choose' | 'limit' | 'done'

export const steps = ['Choose an org', 'Spend limit', 'Ready'] as const

export const invite = {
  org: 'Platform Engineering',
  initials: 'PE',
  inviter: 'Maria Okafor',
  date: '27 Oct',
  group: 'platform-eng',
  role: 'Member',
  agents: 5,
  dailyLimit: '$25.00, set by the org',
}

export const defaults = { orgName: 'Release Engineering', askText: "Release engineering: agents that prepare release notes and cut tags.", limit: '25.00', platformDefault: 25, platformCap: 200, agentDefault: 5 }

export const alertThresholds = [50, 75, 100] as const

export function slugOf(name: string): string {
  return (
    name
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, '-')
      .replace(/^-+|-+$/g, '') || 'my-org'
  )
}

export function doneCopy(path: OnboardingPath, orgName: string, amount: string, alerts: number[], pause: boolean): [string, string] {
  switch (path) {
    case 'join':
      return ['You joined Platform Engineering', 'Its owners set the spend limit, so there is nothing to set up. Five agents already work here.']
    case 'ask':
      return ['Request sent', 'A platform admin will create the org or reply in your inbox. Meanwhile, Idion is yours to use.']
    default: {
      const list = alerts.length === 0 ? 'no alerts' : `alerts at ${alerts.length === 1 ? `${alerts[0]}%` : `${alerts.slice(0, -1).join(', ')} and ${alerts[alerts.length - 1]}%`}`
      return [`${orgName} is ready`, `Its limit is $${amount} a day, with ${list}. ${pause ? 'Agents pause at the limit.' : 'Agents keep running at the limit.'}`]
    }
  }
}

export const daimon = {
  defaultName: 'Daimon',
  abilities: ['Reads and writes your memory, which you can browse, edit and make it forget', 'Sets reminders and schedules that run as you', 'Asks a team agent a question only if that agent opts in, within your role', 'Never uses your org credentials and never acts inside an org itself'],
  budget: '$10.00 a month, paid by the platform',
  model: 'claude-haiku-5.5',
  privacy: 'Private from other people. Platform admins and the audit log can still see it.',
}

export type ChatLine =
  | { id: string; from: 'me'; text: string }
  | { id: string; from: 'daimon'; text: string; meta?: string; actions?: { label: string; href: string; primary?: boolean }[] }

export const chat: ChatLine[] = [
  { id: 'c1', from: 'me', text: "Can you find out when cert-manager 1.19 lands in prod? I don't want to dig through the board." },
  {
    id: 'c2',
    from: 'daimon',
    text: 'I asked LCM Upgrader in Platform Engineering. It is testing 1.19 in staging now and needs your approval for a 30-minute grant before it can finish.',
    meta: 'Asked through ask_agent · LCM Upgrader allows questions from members · cost $0.01, paid by Platform Engineering',
    actions: [
      { label: 'Open the plan approval', href: '/preview/inbox', primary: true },
      { label: 'Open the run', href: '/preview/runs/9c41d07a' },
    ],
  },
  { id: 'c3', from: 'me', text: 'Thanks. Remind me Friday to check it shipped.' },
  { id: 'c4', from: 'daimon', text: `Done: "Check cert-manager 1.19 shipped to prod", Friday 31 Oct at 09:00. I won't keep anything from Platform Engineering in your memory beyond this reminder.` },
]

export const cannedReply = 'Noted. In this preview I do not answer for real, but this is where my reply would appear.'

export const schedules = [
  { id: 's1', name: 'Morning brief', when: 'weekdays 08:00' },
  { id: 's2', name: 'Check cert-manager 1.19 shipped', when: 'Fri 09:00' },
]

export const memory = { notes: ['Prefers summaries as bullet points', 'Travels to Amsterdam on 3 Nov'], count: 12, edited: 'last edited by you yesterday' }

export const budget = { used: 2.14, limit: 10 }

export const reminder = { title: 'Renew the KubeCon EU booking', meta: 'Daimon · due today', hours: 3 }
