import type { AgentState, Job, LinkState } from './api'

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB']

export function formatBytes(bytes: number): string {
  let value = bytes
  let unit = 0
  while (value >= 1000 && unit < UNITS.length - 1) {
    value /= 1000
    unit += 1
  }
  const digits = unit === 0 || value >= 100 ? 0 : value >= 10 ? 1 : 2
  return `${value.toFixed(digits)} ${UNITS[unit]}`
}

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

/** Streams no server or running install holds yet. */
export const streamsLeft = (c: { streamBudget: number; streamsAllocated: number }) =>
  c.streamBudget - c.streamsAllocated

export function linkTone(state: LinkState['state'] | undefined) {
  if (state === 'up') return 'online' as const
  if (state === 'degraded') return 'warn' as const
  if (state === 'down') return 'danger' as const
  return 'idle' as const
}

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
}

/** A job step's name ("disguise") as the step it is ("Check the cover site"). */
export function stepLabel(name: string, kind?: Job['kind']): string {
  if (kind === 'rotate' && name === 'verify') return 'Reconnect with the new keys'
  if (kind === 'update-agent' && name === 'download') return 'Download the agent'
  if (kind === 'update-telemt' && name === 'download') return 'Download the proxy'
  if (kind?.startsWith('update-') && name === 'upload') return 'Send the new version'
  return STEPS[name] ?? name.charAt(0).toUpperCase() + name.slice(1).replace(/[-_]/g, ' ')
}

/** The job failed at the link step: the router could not reach the agent. */
export const failedAtLink = (job: Job) =>
  job.state === 'failed' && job.steps.some((s) => s.name === 'link' && s.state === 'failed')

const EVENTS: Record<string, string> = {
  telemt_down: 'Proxy stopped',
  telemt_up: 'Proxy started',
  telemt_crash_loop: 'Proxy keeps crashing',
  allowlist_violation: 'Refused a destination outside the allowlist',
  dc_dial_failed: 'Could not reach Telegram',
  push_failed: 'Could not apply settings',
  link_up: 'Link up',
  link_down: 'Link down',
}

export const eventLabel = (kind: string) => EVENTS[kind] ?? kind.replace(/_/g, ' ')

/** Why the agent keeps a stopped proxy stopped, if it says. */
export const heldReason = (t: AgentState['telemt'] | undefined): string | null =>
  t && !t.running && t.held ? t.held : null
