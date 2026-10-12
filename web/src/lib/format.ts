import type { AgentState, Job, JobKind, LinkState, Server, User } from './api'

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB']
const MONTHS = 'JanFebMarAprMayJunJulAugSepOctNovDec'
export const DAY = 86400e3
const pad = (n: number) => String(n).padStart(2, '0')
const month = (i: number) => MONTHS.slice(i * 3, i * 3 + 3)

export function formatBytes(bytes: number): string {
  let value = bytes
  let unit = 0
  while (value >= 1000 && unit < UNITS.length - 1) {
    value /= 1000
    unit += 1
  }
  const digits = unit === 0 || value >= 100 ? 0 : 1
  return `${value.toFixed(digits)} ${UNITS[unit]}`
}

export const formatRate = (bytesPerSec: number) => `${formatBytes(bytesPerSec)}/s`

export function formatDuration(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds))
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  if (d > 0) return `${d}d ${h}h`
  if (h > 0) return `${h}h ${m}m`
  if (m > 0) return `${m}m`
  return `${s}s`
}

/** A stopwatch: "2:14", "1:02:09". */
export function formatClock(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds))
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  return `${h ? `${h}:${pad(m)}` : m}:${pad(s % 60)}`
}

export const formatCount = (n: number) => n.toLocaleString('en-US')

/** "1 prefix", "12 prefixes". */
export const plural = (n: number, one: string, many = `${one}s`) =>
  `${formatCount(n)} ${n === 1 ? one : many}`

/** "in 3 days", "2 hours ago"… relative to `now`. */
export function relativeTime(iso: string, now = Date.now()): string {
  const diff = new Date(iso).getTime() - now
  const abs = Math.abs(diff)
  if (diff < 0 && abs < 60000) return 'just now'
  const [n, unit] =
    abs >= 86400000
      ? [Math.round(abs / 86400000), 'day']
      : abs >= 3600000
        ? [Math.round(abs / 3600000), 'hour']
        : [Math.max(1, Math.round(abs / 60000)), 'minute']
  const phrase = `${n} ${unit}${n === 1 ? '' : 's'}`
  return diff >= 0 ? `in ${phrase}` : `${phrase} ago`
}

/** "12 s ago" for the last few minutes, then like relativeTime. */
export const secondsAgo = (iso: string, now = Date.now()) => {
  const s = Math.max(0, Math.round((now - Date.parse(iso)) / 1000))
  return s < 120 ? `${s} s ago` : relativeTime(iso, now)
}

/** Local "07:12" today, else "3 Oct". */
export function clockOrDay(iso: string, now = Date.now()): string {
  const d = new Date(iso)
  return d.toDateString() === new Date(now).toDateString()
    ? `${pad(d.getHours())}:${pad(d.getMinutes())}`
    : `${d.getDate()} ${month(d.getMonth())}`
}

/** A UTC day as "1 Nov", with the year when it is not this one. */
export function dayText(iso: string, now = Date.now()): string {
  const d = new Date(iso)
  const y = d.getUTCFullYear()
  return `${d.getUTCDate()} ${month(d.getUTCMonth())}${y === new Date(now).getUTCFullYear() ? '' : ` ${y}`}`
}

export const GB = 1_000_000_000

/** Parses an optional positive number field; '' means "no limit" (0). */
export function parseLimit(text: string, scale = 1): number | 'invalid' {
  const t = text.trim()
  if (t === '') return 0
  const n = Number(t)
  if (!Number.isFinite(n) || n <= 0) return 'invalid'
  return Math.round(n * scale)
}

/** A limit back into its form text; 0 is "no limit". */
export function limitText(value: number | null | undefined, scale = 1): string {
  if (!value) return ''
  const n = value / scale
  return Number.isInteger(n) ? String(n) : String(Number(n.toFixed(3)))
}

const USERNAME = /^[A-Za-z0-9_.-]{1,32}$/

// a name of only dots cannot travel in a URL path
export const validUserName = (name: string) => USERNAME.test(name) && !/^\.+$/.test(name)
export const DOMAIN = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$/i

/** A secret someone already has: 32 hex, dd + 32 hex, or ee + 32 hex + the hex of a domain. */
export const validSecret = (s: string) =>
  /^(dd)?[0-9a-f]{32}$/i.test(s) || /^ee[0-9a-f]{32}([0-9a-f]{2})+$/i.test(s)

/** Streams no server or running install holds yet. */
export const streamsLeft = (c: { streamBudget: number; streamsAllocated: number }) =>
  c.streamBudget - c.streamsAllocated

export function linkTone(state: LinkState['state'] | undefined) {
  if (state === 'up') return 'online' as const
  if (state === 'degraded') return 'warn' as const
  if (state === 'down') return 'danger' as const
  return 'idle' as const
}

// --- quotas and expiry -------------------------------------------------------

/** Share of a quota used, 0–100; it reads 100 only once it is all used. */
export const quotaPercent = (used: number, quota: number) =>
  used >= quota ? 100 : Math.min(99, Math.round((used / quota) * 100))

/** "3.9 of 50 GB · 8%". */
export function quotaText(used: number, quota: number): string {
  const [scale, unit] = quota >= GB ? [GB, 'GB'] : [1e6, 'MB']
  const q = quota / scale
  return `${(used / scale).toFixed(1)} of ${Number.isInteger(q) ? q : q.toFixed(1)} ${unit} · ${quotaPercent(used, quota)}%`
}

/** The fill colour of a usage bar: amber from 80%, red when used up. */
export const barTone = (pct: number) => (pct >= 100 ? 'bg-bad' : pct >= 80 ? 'bg-warn' : 'bg-info')

const isoSec = (ms: number) => new Date(ms).toISOString().replace(/\.\d+Z$/, 'Z')

/** The end of the UTC day `days` from today: expiry presets end on a day, like the date picker. */
export const daysFromToday = (days: number, now = Date.now()) =>
  `${new Date(now + days * DAY).toISOString().slice(0, 10)}T23:59:59Z`

/** A renewal: from today when already expired, else from the current expiry; none stays none. */
export function renewed(expiresAt: string | null, days = 30, now = Date.now()): string | null {
  if (!expiresAt) return null
  const t = Date.parse(expiresAt)
  return t < now ? daysFromToday(days, now) : isoSec(t + days * DAY)
}

export type ExpiryTone = 'never' | 'normal' | 'soon' | 'past'

/** An expiry as a day plus how far off, and as one line for phones. */
export function expiry(iso: string | null, now = Date.now()) {
  if (!iso) return { tone: 'never' as ExpiryTone, day: 'Never', rel: '', line: 'Never expires' }
  const t = Date.parse(iso)
  const d = new Date(t)
  const today = d.toISOString().slice(0, 10) === new Date(now).toISOString().slice(0, 10)
  const time = `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}`
  const day = today ? `Today ${time}` : dayText(iso, now)
  const rel = relativeTime(iso, now)
  const past = t < now
  return {
    tone: (past ? 'past' : t - now <= 7 * DAY ? 'soon' : 'normal') as ExpiryTone,
    day,
    rel,
    line: `${past ? 'Expired' : 'Expires'} ${today ? `today at ${time}` : day}, ${rel}`,
  }
}

// --- users -------------------------------------------------------------------

export type StatusKey = 'online' | 'active' | 'paused' | 'expired' | 'quota' | 'unknown'

/** One status per user: the switch only pauses, so the badge carries the rest. */
export function userStatus(u: User, linkDown: boolean, now = Date.now()): { key: StatusKey; label: string; sub: string } {
  const seen = u.lastSeenAt ? `last seen ${relativeTime(u.lastSeenAt, now)}` : 'never connected'
  if (u.state === 'disabled') return { key: 'paused', label: 'Paused', sub: seen }
  if (u.state === 'expired') return { key: 'expired', label: 'Expired', sub: seen }
  if (u.state === 'over-quota') return { key: 'quota', label: 'Quota used', sub: seen }
  if (linkDown) return { key: 'unknown', label: 'Unknown', sub: u.lastSeenAt ? 'server unreachable' : seen }
  if (u.online) return { key: 'online', label: 'Online', sub: plural(u.connections ?? 0, 'connection') }
  return { key: 'active', label: 'Active', sub: seen }
}

export type Filter = 'all' | 'online' | 'soon' | 'nearly' | 'paused' | 'expired' | 'quota'

export const FILTERS: [Filter, string][] = [
  ['all', 'All'],
  ['online', 'Online'],
  ['soon', 'Expiring soon'],
  ['nearly', 'Quota nearly used'],
  ['paused', 'Paused'],
  ['expired', 'Expired'],
  ['quota', 'Quota used'],
]

export function matchesFilter(u: User, f: Filter, now = Date.now()): boolean {
  if (f === 'online') return u.online === true
  if (f === 'soon') return expiry(u.expiresAt, now).tone === 'soon'
  if (f === 'nearly') {
    const p = u.quotaBytes ? quotaPercent(u.up + u.down, u.quotaBytes) : 0
    return p >= 80 && p < 100
  }
  if (f === 'paused') return !u.enabled
  if (f === 'expired') return u.state === 'expired'
  if (f === 'quota') return u.state === 'over-quota'
  return true
}

export type Sort = 'name' | 'usage' | 'expiry' | 'seen'

export const SORTS: [Sort, string][] = [
  ['name', 'Name'],
  ['usage', 'Usage'],
  ['expiry', 'Expiry'],
  ['seen', 'Last seen'],
]

const SORT_KEY: Record<Sort, (u: User) => number> = {
  name: () => 0,
  // most traffic in 30 days first
  usage: (u) => -(u.monthUp + u.monthDown),
  // soonest (or already past) first, none last
  expiry: (u) => (u.expiresAt ? Date.parse(u.expiresAt) : Infinity),
  // online first, then most recently seen, never last
  seen: (u) => (u.online ? -Infinity : u.lastSeenAt ? -Date.parse(u.lastSeenAt) : Infinity),
}

export const sortUsers = (list: User[], by: Sort) =>
  [...list].sort((a, b) => SORT_KEY[by](a) - SORT_KEY[by](b) || a.name.localeCompare(b.name))

/** "bob and eve", "a, b, c and 4 more". */
export function nameList(names: string[], max = 3): string {
  if (names.length < 2) return names.join('')
  if (names.length <= max) return `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}`
  return `${names.slice(0, max).join(', ')} and ${names.length - max} more`
}

// --- servers -----------------------------------------------------------------

export type Tone = 'online' | 'warn' | 'danger' | 'idle'

/** A server's health for its card: a tone, a word, and the reason when it is not fine. */
export function health(s: Server, now = Date.now()): { tone: Tone; label: string; reason: string } {
  const a = s.agent
  const t = a?.telemt
  const attention = (tone: Tone, reason: string) => ({ tone, label: 'Needs attention', reason })
  if (!s.registered) return { tone: 'danger', label: 'Not finished', reason: 'The install never finished' }
  if (s.link.state === 'down')
    return {
      tone: 'danger',
      label: 'Unreachable',
      reason: s.lastSeenAt
        ? `Not reachable since ${clockOrDay(s.lastSeenAt, now)} (${relativeTime(s.lastSeenAt, now)})`
        : 'Not reachable',
    }
  if (t?.crashLoop) return attention('danger', 'The proxy keeps crashing')
  if (s.proxyStopped) return { tone: 'warn', label: 'Stopped', reason: 'You stopped the proxy' }
  if (a && !a.capture.rules) return attention('danger', 'Telegram traffic rules are missing')
  if (t && !t.running) return attention('warn', 'The proxy is not running')
  if (a?.cover.checkedAt && !a.cover.ok) return attention('warn', "The cover site isn't working")
  if (!s.sync.healthy && s.sync.lastPushError) return attention('warn', "Settings aren't applied yet")
  if (s.link.state === 'degraded') return attention('warn', `${s.link.up} of ${s.link.total} link connections are up`)
  if (!a) return { tone: 'idle', label: 'Starting', reason: "Waiting for the agent's first report" }
  return { tone: 'online', label: 'Working', reason: '' }
}

/** Connections a restart drops: what users have open, or the link's count from an older agent. */
export const openConnections = (s: Server) => (s.usage.live ? s.usage.connections : s.link.streams)

/** The words after the number in a confirmation: who a restart drops. */
export function dropLine(s: Server): string {
  const n = openConnections(s)
  if (!n) return 'open connections right now, so nobody is cut off.'
  const who = s.usage.live && s.usage.usersOnline ? `, from ${plural(s.usage.usersOnline, 'person', 'people')} online` : ''
  return `open ${n === 1 ? 'connection drops' : 'connections drop'}${who}. Telegram reconnects on its own in a few seconds.`
}

/** The proxy is back after a restart: a newer report shows it running, started since the click. */
export function restartDone(s: Server, before: string | undefined, sinceMs: number): boolean {
  const a = s.agent
  return !!a && a.reportedAt !== before && a.telemt.running && a.telemt.uptimeSec * 1000 <= sinceMs + 5000
}

/** Why the agent keeps a stopped proxy stopped, if it says. */
export const heldReason = (t: AgentState['telemt'] | undefined): string | null =>
  t && !t.running && t.held ? t.held : null

// --- logs --------------------------------------------------------------------

const STAMP = /^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:?\d\d)?)\s+/
const SOURCE = /^(agent|telemt):\s+/
const ERROR = /\b(error|fatal|panic|failed|cannot|can't|exited|denied)\b/i
const WARN = /\b(warn|warning|keeps crashing|backing off|waiting|timed? ?out)\b/i

export type LogTone = 'error' | 'warn' | ''

/** A log line split into its time, source and message, with a tone for tinting. */
export function parseLogLine(line: string): { at: string | null; src: string; msg: string; tone: LogTone } {
  let msg = line
  const t = STAMP.exec(msg)
  if (t) msg = msg.slice(t[0].length)
  const s = SOURCE.exec(msg)
  if (s) msg = msg.slice(s[0].length)
  return { at: t ? t[1] : null, src: s ? s[1] : '', msg, tone: ERROR.test(msg) ? 'error' : WARN.test(msg) ? 'warn' : '' }
}

// these only say that it crashed again, not why
const EXIT_NOTE = /exited|exit status|restart \d|keeps crashing|backing off|waiting/i

/** Why the proxy keeps crashing, from the newest error line that names a cause. */
export function crashCause(lines: string[]): { text: string; at: string | null } | null {
  const errors = lines.map(parseLogLine).filter((l) => l.tone === 'error').reverse()
  const l = errors.find((e) => !EXIT_NOTE.test(e.msg)) ?? errors[0]
  if (!l) return null
  if (/address already in use/i.test(l.msg)) {
    const port = /:(\d+)\D*address already in use/i.exec(l.msg)?.[1] ?? '443'
    return { text: `Port ${port} is taken by another program on the server.`, at: l.at }
  }
  const text = l.msg.charAt(0).toUpperCase() + l.msg.slice(1)
  return { text: /[.!]$/.test(text) ? text : `${text}.`, at: l.at }
}

// --- jobs --------------------------------------------------------------------

const STEPS: Record<string, string> = {
  download: 'Download the agent and proxy',
  check: 'Check the server',
  verify: 'Check the downloads',
  nftables: 'Install nftables',
  account: 'Create the proxy account',
  upload: 'Upload the agent and proxy',
  directories: 'Create the directories',
  disguise: 'Check the cover site',
  credentials: 'Write the link keys',
  service: 'Start the agent',
  register: 'Save the server',
  link: 'Connect to the agent',
  restart: 'Restart the agent',
  commit: 'Confirm the update',
  rotate: 'Switch to new keys',
  remove: 'Remove the agent',
  forget: 'Forget the server',
  open: 'Open the new port',
  switch: 'Move the link over',
  close: 'Close the old port',
}

/** A job step's name ("disguise") as the step it is ("Check the cover site"). */
export function stepLabel(name: string, kind?: JobKind): string {
  if (kind === 'rotate' && name === 'verify') return 'Reconnect with the new keys'
  if (kind === 'update-agent' && name === 'download') return 'Download the agent'
  if (kind === 'update-telemt' && name === 'download') return 'Download the proxy'
  if (kind?.startsWith('update-') && name === 'upload') return 'Send the new version'
  return STEPS[name] ?? name.charAt(0).toUpperCase() + name.slice(1).replace(/[-_]/g, ' ')
}

const JOBS: Record<JobKind, [string, string, string]> = {
  install: ['Installing', 'Server added', 'Installation failed'],
  'update-agent': ['Updating the agent', 'Agent updated', 'Agent update failed'],
  'update-telemt': ['Updating the proxy', 'Proxy updated', 'Proxy update failed'],
  rotate: ['Renewing link keys', 'Link keys renewed', 'Renewing link keys failed'],
  repair: ['Repairing', 'Server repaired', 'Repair failed'],
  uninstall: ['Removing', 'Server removed', 'Removal failed'],
  'link-port': ['Changing the link port', 'Link port changed', 'Link port change failed'],
}

/** What a job is doing, or how it ended. */
export const jobTitle = (j: Pick<Job, 'kind' | 'state'>) =>
  JOBS[j.kind]?.[j.state === 'running' ? 0 : j.state === 'ok' ? 1 : 2] ?? j.kind

/** "Step 5 of 11": the running step, else the next one after those done. */
export function jobStep(job: Job): { n: number; m: number } {
  const i = job.steps.findIndex((s) => s.state === 'running')
  const done = job.steps.filter((s) => s.state === 'ok').length
  return { n: i >= 0 ? i + 1 : Math.min(done + 1, job.steps.length), m: job.steps.length }
}

/** The share done of a step whose detail starts with a percentage ("38% · …"). */
export function stepPercent(detail?: string): number | null {
  const m = detail ? /^(\d{1,3})%/.exec(detail) : null
  return m ? Math.min(100, Number(m[1])) : null
}

/** The job failed at the link step: the router could not reach the agent. */
export const failedAtLink = (job: Job) =>
  job.state === 'failed' && job.steps.some((s) => s.name === 'link' && s.state === 'failed')

const EVENTS: Record<string, string> = {
  telemt_down: 'Proxy stopped',
  telemt_up: 'Proxy started',
  telemt_crash_loop: 'Proxy keeps crashing',
  allowlist_violation: 'Refused a non-Telegram address',
  dc_dial_failed: "Couldn't reach Telegram",
  push_failed: "Couldn't apply settings",
  link_up: 'Link up',
  link_down: 'Link connection down',
}

export const eventLabel = (kind: string) => EVENTS[kind] ?? kind.replace(/_/g, ' ')
