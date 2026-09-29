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

/** "in 3 days", "2 hours ago"… relative to `now`. */
export function relativeTime(iso: string, now = Date.now()): string {
  const diff = new Date(iso).getTime() - now
  const abs = Math.abs(diff)
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
export const MBIT = 1_000_000

/** Parses an optional positive number field; '' means "no limit" (null). */
export function parseLimit(text: string, scale = 1): number | null | 'invalid' {
  const t = text.trim()
  if (t === '') return null
  const n = Number(t)
  if (!Number.isFinite(n) || n <= 0) return 'invalid'
  return Math.round(n * scale)
}

/** A limit back into its form text. */
export function limitText(value: number | null, scale = 1): string {
  if (value === null || value === undefined) return ''
  const n = value / scale
  return Number.isInteger(n) ? String(n) : String(Number(n.toFixed(3)))
}

/** tg://proxy?… → https://t.me/proxy?… (opens in a browser too). */
export function toWebLink(tgLink: string): string {
  return tgLink.replace(/^tg:\/\/proxy\?/, 'https://t.me/proxy?')
}

export const USERNAME = /^[A-Za-z0-9_.-]{1,64}$/
