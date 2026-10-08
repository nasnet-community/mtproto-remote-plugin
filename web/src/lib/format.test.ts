import { describe, expect, it } from 'vitest'
import type { Job } from './api'
import {
  DOMAIN,
  GB,
  eventLabel,
  failedAtLink,
  formatBytes,
  formatCount,
  formatDuration,
  heldReason,
  limitText,
  linkTone,
  parseLimit,
  plural,
  relativeTime,
  stepLabel,
  streamsLeft,
  validUserName,
} from './format'

const job = (o: Partial<Job>): Job => ({
  id: 'j1',
  kind: 'install',
  state: 'running',
  steps: [],
  startedAt: '2026-10-01T00:00:00Z',
  ...o,
})

describe('format', () => {
  it('formats bytes with sensible precision', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(999)).toBe('999 B')
    expect(formatBytes(1_500)).toBe('1.50 KB')
    expect(formatBytes(12_345_678)).toBe('12.3 MB')
    expect(formatBytes(5 * GB)).toBe('5.00 GB')
  })

  it('formats durations and counts', () => {
    expect(formatDuration(42)).toBe('42s')
    expect(formatDuration(3_720)).toBe('1h 2m')
    expect(formatDuration(90_000)).toBe('1d 1h')
    expect(formatCount(1_884_201)).toBe('1,884,201')
    expect(plural(1, 'prefix', 'prefixes')).toBe('1 prefix')
    expect(plural(12, 'prefix', 'prefixes')).toBe('12 prefixes')
    expect(plural(2000, 'user')).toBe('2,000 users')
  })

  it('describes times relative to now', () => {
    const now = Date.parse('2026-09-29T12:00:00Z')
    expect(relativeTime('2026-10-02T12:00:00Z', now)).toBe('in 3 days')
    expect(relativeTime('2026-09-29T10:00:00Z', now)).toBe('2 hours ago')
    expect(relativeTime('2026-09-29T12:00:20Z', now)).toBe('in 1 minute')
    expect(relativeTime('2026-09-29T11:59:40Z', now)).toBe('just now')
    expect(relativeTime('2026-09-29T11:58:40Z', now)).toBe('1 minute ago')
  })

  it('parses optional limits, where empty and 0 mean none', () => {
    expect(parseLimit('')).toBe(0)
    expect(parseLimit(' 5 ', GB)).toBe(5 * GB)
    expect(parseLimit('1.5', GB)).toBe(1_500_000_000)
    expect(parseLimit('0')).toBe('invalid')
    expect(parseLimit('-5')).toBe('invalid')
    expect(parseLimit('abc')).toBe('invalid')
    expect(limitText(0)).toBe('')
    expect(limitText(null)).toBe('')
    expect(limitText(5 * GB, GB)).toBe('5')
    expect(limitText(1_500_000_000, GB)).toBe('1.5')
  })

  it('checks user names and domains', () => {
    expect(validUserName('alice.b_c-1')).toBe(true)
    expect(validUserName('a'.repeat(32))).toBe(true)
    expect(validUserName('a'.repeat(33))).toBe(false)
    expect(validUserName('bad name')).toBe(false)
    expect(validUserName('..')).toBe(false)
    expect(validUserName('')).toBe(false)
    expect(DOMAIN.test('www.example.com')).toBe(true)
    expect(DOMAIN.test('Example.ORG')).toBe(true)
    expect(DOMAIN.test('localhost')).toBe(false)
    expect(DOMAIN.test('not a domain')).toBe(false)
  })

  it('maps link states to tones', () => {
    expect(linkTone('up')).toBe('online')
    expect(linkTone('degraded')).toBe('warn')
    expect(linkTone('down')).toBe('danger')
    expect(linkTone(undefined)).toBe('idle')
  })

  it('names job steps for people, by job kind', () => {
    expect(stepLabel('disguise')).toBe('Check the cover site')
    expect(stepLabel('link', 'install')).toBe('Connect to the agent')
    expect(stepLabel('verify', 'install')).toBe('Check the downloads')
    expect(stepLabel('verify', 'rotate')).toBe('Reconnect with the new keys')
    expect(stepLabel('upload', 'update-agent')).toBe('Send the new version')
    expect(stepLabel('download', 'update-telemt')).toBe('Download the proxy')
    expect(stepLabel('something-new')).toBe('Something new')
  })

  it('spots a job that failed at the link step', () => {
    const steps = [
      { name: 'register', state: 'ok' as const },
      { name: 'link', state: 'failed' as const, detail: 'cannot reach it' },
    ]
    expect(failedAtLink(job({ state: 'failed', steps }))).toBe(true)
    expect(failedAtLink(job({ state: 'failed', steps: [{ name: 'check', state: 'failed' }] }))).toBe(false)
    expect(failedAtLink(job({ state: 'ok', steps: [{ name: 'link', state: 'ok' }] }))).toBe(false)
  })

  it('names server events', () => {
    expect(eventLabel('telemt_crash_loop')).toBe('Proxy keeps crashing')
    expect(eventLabel('allowlist_violation')).toBe('Refused a destination outside the allowlist')
    expect(eventLabel('new_kind')).toBe('new kind')
  })

  it('counts the streams nobody holds, running installs included', () => {
    expect(streamsLeft({ streamBudget: 6000, streamsAllocated: 5500 })).toBe(500)
    expect(streamsLeft({ streamBudget: 6000, streamsAllocated: 6500 })).toBe(-500)
  })
})

describe('heldReason', () => {
  const t = { running: false, restarts: 0, uptimeSec: 0, lastExit: '', crashLoop: false }

  it('says why the agent keeps the proxy stopped', () => {
    expect(heldReason({ ...t, held: "waiting for the router's first state" })).toBe("waiting for the router's first state")
  })

  it('has nothing to say while it runs, or without a reason', () => {
    expect(heldReason({ ...t, running: true, held: 'stale' })).toBeNull()
    expect(heldReason({ ...t, held: '' })).toBeNull()
    expect(heldReason(t)).toBeNull()
    expect(heldReason(undefined)).toBeNull()
  })
})
