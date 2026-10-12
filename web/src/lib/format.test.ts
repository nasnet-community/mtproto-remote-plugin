import { describe, expect, it } from 'vitest'
import type { Job, Server, User } from './api'
import {
  DOMAIN,
  GB,
  barTone,
  clockOrDay,
  crashCause,
  dayText,
  daysFromToday,
  dropLine,
  eventLabel,
  expiry,
  failedAtLink,
  formatBytes,
  formatClock,
  formatCount,
  formatDuration,
  formatRate,
  health,
  heldReason,
  jobStep,
  jobTitle,
  limitText,
  linkTone,
  matchesFilter,
  nameList,
  openConnections,
  parseLimit,
  parseLogLine,
  plural,
  quotaPercent,
  quotaText,
  relativeTime,
  renewed,
  restartDone,
  secondsAgo,
  sortUsers,
  stepLabel,
  stepPercent,
  streamsLeft,
  userStatus,
  validSecret,
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

const NOW = Date.parse('2026-10-11T12:00:00Z')

const user = (o: Partial<User> = {}): User => ({
  name: 'alice',
  enabled: true,
  state: 'active',
  maxIps: 0,
  expiresAt: null,
  quotaBytes: 0,
  up: 0,
  down: 0,
  createdAt: '2026-09-01T00:00:00Z',
  secret: '0'.repeat(32),
  note: '',
  online: false,
  connections: 0,
  devices: 0,
  lastSeenAt: null,
  todayUp: 0,
  todayDown: 0,
  monthUp: 0,
  monthDown: 0,
  links: { tg: 'tg://x', https: 'https://t.me/x', extra: [] },
  ...o,
})

const usage = { live: true, usersOnline: 0, connections: 0, devices: 0, rateUp: 0, rateDown: 0, todayUp: 0, todayDown: 0, monthUp: 0, monthDown: 0, updatedAt: null }

const server = (o: Partial<Server> = {}): Server =>
  ({
    id: 1,
    name: 'Frankfurt VPS',
    registered: true,
    lastSeenAt: '2026-10-11T09:00:00Z',
    link: { state: 'up', up: 2, total: 2, streams: 9, relayed: 0, dialFailures: 0, violations: 0, bytesToDc: 0, bytesFromDc: 0, conns: [] },
    agent: {
      version: '0.5.0',
      telemtVersion: '3.4.25',
      uptimeSec: 100,
      rssBytes: 0,
      stateRev: 'r1',
      reportedAt: '2026-10-11T11:59:55Z',
      telemt: { running: true, restarts: 0, uptimeSec: 100, lastExit: '', crashLoop: false },
      capture: { rules: true, active: 0, rejected: 0 },
      cover: { domain: 'www.example.com', ok: true, checkedAt: '2026-10-11T11:00:00Z', error: '' },
    },
    sync: { desiredRev: 'r1', appliedRev: 'r1', lastPushAt: null, lastPushError: '', healthy: true },
    proxyStopped: false,
    usage,
    ...o,
  }) as Server

describe('format', () => {
  it('formats bytes, rates and clocks', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(999)).toBe('999 B')
    expect(formatBytes(1_500)).toBe('1.5 KB')
    expect(formatBytes(12_345_678)).toBe('12.3 MB')
    expect(formatBytes(5 * GB)).toBe('5.0 GB')
    expect(formatBytes(4.2 * GB)).toBe('4.2 GB')
    expect(formatRate(1_200_000)).toBe('1.2 MB/s')
    expect(formatClock(0)).toBe('0:00')
    expect(formatClock(134)).toBe('2:14')
    expect(formatClock(3729)).toBe('1:02:09')
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
    expect(secondsAgo('2026-09-29T11:59:50Z', now)).toBe('10 s ago')
    expect(secondsAgo('2026-09-29T11:50:00Z', now)).toBe('10 minutes ago')
  })

  it('names days in UTC and clock times locally', () => {
    expect(dayText('2026-11-01T23:59:59Z', NOW)).toBe('1 Nov')
    expect(dayText('2027-01-05T00:00:00Z', NOW)).toBe('5 Jan 2027')
    const local = new Date(NOW - 3600e3)
    expect(clockOrDay(local.toISOString(), NOW)).toBe(
      `${String(local.getHours()).padStart(2, '0')}:${String(local.getMinutes()).padStart(2, '0')}`,
    )
    expect(clockOrDay('2026-10-03T12:00:00Z', NOW)).toBe('3 Oct')
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

  it('checks user names, domains and existing secrets', () => {
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
    const hex = '0123456789abcdef'.repeat(2)
    expect(validSecret(hex)).toBe(true)
    expect(validSecret(`dd${hex}`)).toBe(true)
    expect(validSecret(`ee${hex}${Buffer.from('a.io').toString('hex')}`)).toBe(true)
    expect(validSecret(`ee${hex}`)).toBe(false)
    expect(validSecret(hex.slice(1))).toBe(false)
    expect(validSecret(`${hex}zz`)).toBe(false)
  })

  it('maps link states to tones', () => {
    expect(linkTone('up')).toBe('online')
    expect(linkTone('degraded')).toBe('warn')
    expect(linkTone('down')).toBe('danger')
    expect(linkTone(undefined)).toBe('idle')
  })

  it('counts the streams nobody holds, running installs included', () => {
    expect(streamsLeft({ streamBudget: 6000, streamsAllocated: 5500 })).toBe(500)
    expect(streamsLeft({ streamBudget: 6000, streamsAllocated: 6500 })).toBe(-500)
  })
})

describe('quotas and expiry', () => {
  it('shows a quota as used of total with a percentage', () => {
    expect(quotaText(3.9 * GB, 50 * GB)).toBe('3.9 of 50 GB · 8%')
    expect(quotaText(84 * GB, 100 * GB)).toBe('84.0 of 100 GB · 84%')
    expect(quotaText(10 * GB, 10 * GB)).toBe('10.0 of 10 GB · 100%')
    expect(quotaText(250e6, 500e6)).toBe('250.0 of 500 MB · 50%')
    expect(quotaText(1e9, 2.5e9)).toBe('1.0 of 2.5 GB · 40%')
  })

  it('reads 100% only once the quota is all used', () => {
    expect(quotaPercent(9.96 * GB, 10 * GB)).toBe(99)
    expect(quotaPercent(10 * GB, 10 * GB)).toBe(100)
    expect(quotaPercent(12 * GB, 10 * GB)).toBe(100)
    expect(barTone(79)).toBe('bg-info')
    expect(barTone(80)).toBe('bg-warn')
    expect(barTone(100)).toBe('bg-bad')
  })

  it('ends presets and renewals on a UTC day', () => {
    expect(daysFromToday(30, NOW)).toBe('2026-11-10T23:59:59Z')
    // expired: from today
    expect(renewed('2026-10-05T23:59:59Z', 30, NOW)).toBe('2026-11-10T23:59:59Z')
    // still valid: from the current expiry
    expect(renewed('2026-11-01T23:59:59Z', 30, NOW)).toBe('2026-12-01T23:59:59Z')
    expect(renewed(null, 30, NOW)).toBeNull()
  })

  it('describes an expiry for the table and for phones', () => {
    expect(expiry(null, NOW)).toMatchObject({ tone: 'never', day: 'Never', line: 'Never expires' })
    expect(expiry('2026-11-01T23:59:59Z', NOW)).toMatchObject({ tone: 'normal', day: '1 Nov', rel: 'in 21 days', line: 'Expires 1 Nov, in 21 days' })
    expect(expiry('2026-10-11T23:59:59Z', NOW)).toMatchObject({ tone: 'soon', day: 'Today 23:59', line: 'Expires today at 23:59, in 12 hours' })
    expect(expiry('2026-10-05T23:59:59Z', NOW)).toMatchObject({ tone: 'past', day: '5 Oct', line: 'Expired 5 Oct, 6 days ago' })
  })
})

describe('users', () => {
  it('gives each user one status, with the switch only meaning paused', () => {
    expect(userStatus(user({ online: true, connections: 3 }), false, NOW)).toEqual({ key: 'online', label: 'Online', sub: '3 connections' })
    expect(userStatus(user({ lastSeenAt: '2026-10-11T11:48:00Z' }), false, NOW)).toEqual({ key: 'active', label: 'Active', sub: 'last seen 12 minutes ago' })
    expect(userStatus(user({ enabled: false, state: 'disabled' }), false, NOW)).toMatchObject({ key: 'paused', sub: 'never connected' })
    expect(userStatus(user({ state: 'expired' }), false, NOW)).toMatchObject({ key: 'expired', label: 'Expired' })
    expect(userStatus(user({ state: 'over-quota' }), false, NOW)).toMatchObject({ key: 'quota', label: 'Quota used' })
    // with the server unreachable, nobody can tell who is online
    expect(userStatus(user({ online: null, lastSeenAt: '2026-10-11T09:00:00Z' }), true, NOW)).toEqual({ key: 'unknown', label: 'Unknown', sub: 'server unreachable' })
    expect(userStatus(user({ online: null }), true, NOW)).toMatchObject({ key: 'unknown', sub: 'never connected' })
  })

  it('filters users', () => {
    expect(matchesFilter(user({ online: true }), 'online', NOW)).toBe(true)
    expect(matchesFilter(user({ online: null }), 'online', NOW)).toBe(false)
    expect(matchesFilter(user({ expiresAt: '2026-10-17T23:59:59Z' }), 'soon', NOW)).toBe(true)
    expect(matchesFilter(user({ expiresAt: '2026-10-30T23:59:59Z' }), 'soon', NOW)).toBe(false)
    expect(matchesFilter(user({ expiresAt: '2026-10-01T23:59:59Z', state: 'expired' }), 'soon', NOW)).toBe(false)
    expect(matchesFilter(user({ quotaBytes: 100, up: 40, down: 44 }), 'nearly', NOW)).toBe(true)
    expect(matchesFilter(user({ quotaBytes: 100, up: 50, down: 50, state: 'over-quota' }), 'nearly', NOW)).toBe(false)
    expect(matchesFilter(user({ quotaBytes: 100, up: 50, down: 50, state: 'over-quota' }), 'quota', NOW)).toBe(true)
    expect(matchesFilter(user({ enabled: false, state: 'disabled' }), 'paused', NOW)).toBe(true)
    expect(matchesFilter(user({ state: 'expired' }), 'expired', NOW)).toBe(true)
    expect(matchesFilter(user(), 'all', NOW)).toBe(true)
  })

  it('sorts users by name, usage, expiry and last seen', () => {
    const list = [
      user({ name: 'carol', monthUp: 5, lastSeenAt: '2026-10-10T00:00:00Z', expiresAt: '2026-12-01T00:00:00Z' }),
      user({ name: 'alice', monthUp: 50, online: true }),
      user({ name: 'bob', monthDown: 20, lastSeenAt: '2026-10-11T00:00:00Z', expiresAt: '2026-10-20T00:00:00Z' }),
      user({ name: 'dan' }),
    ]
    const names = (by: Parameters<typeof sortUsers>[1]) => sortUsers(list, by).map((u) => u.name)
    expect(names('name')).toEqual(['alice', 'bob', 'carol', 'dan'])
    expect(names('usage')).toEqual(['alice', 'bob', 'carol', 'dan'])
    expect(names('expiry')).toEqual(['bob', 'carol', 'alice', 'dan'])
    expect(names('seen')).toEqual(['alice', 'bob', 'carol', 'dan'])
  })

  it('lists names in a sentence', () => {
    expect(nameList(['bob'])).toBe('bob')
    expect(nameList(['bob', 'eve'])).toBe('bob and eve')
    expect(nameList(['a', 'b', 'c'])).toBe('a, b and c')
    expect(nameList(['a', 'b', 'c', 'd', 'e'])).toBe('a, b, c and 2 more')
  })
})

describe('servers', () => {
  it('sums up health for the card', () => {
    expect(health(server(), NOW)).toEqual({ tone: 'online', label: 'Working', reason: '' })
    expect(health(server({ link: { ...server().link, state: 'down', up: 0 } }), NOW)).toMatchObject({ tone: 'danger', label: 'Unreachable' })
    expect(health(server({ link: { ...server().link, state: 'down', up: 0 } }), NOW).reason).toMatch(/^Not reachable since .+ \(3 hours ago\)$/)
    const crashing = server()
    crashing.agent!.telemt = { ...crashing.agent!.telemt, running: false, crashLoop: true }
    expect(health(crashing, NOW)).toEqual({ tone: 'danger', label: 'Needs attention', reason: 'The proxy keeps crashing' })
    expect(health(server({ proxyStopped: true }), NOW)).toMatchObject({ tone: 'warn', label: 'Stopped' })
    expect(health(server({ link: { ...server().link, state: 'degraded', up: 1 } }), NOW).reason).toBe('1 of 2 link connections are up')
    expect(health(server({ registered: false }), NOW).label).toBe('Not finished')
  })

  it('counts the connections a restart drops', () => {
    expect(openConnections(server({ usage: { ...usage, connections: 14, usersOnline: 6 } }))).toBe(14)
    // an older agent can't say, so the link's count stands in
    expect(openConnections(server({ usage: { ...usage, live: false } }))).toBe(9)
    expect(dropLine(server({ usage: { ...usage, connections: 14, usersOnline: 6 } }))).toBe(
      'open connections drop, from 6 people online. Telegram reconnects on its own in a few seconds.',
    )
    expect(dropLine(server({ usage: { ...usage, live: false }, link: { ...server().link, streams: 1 } }))).toMatch(/^open connection drops\. /)
    expect(dropLine(server())).toBe('open connections right now, so nobody is cut off.')
  })

  it('knows a restarted proxy is back from a newer report', () => {
    const s = server()
    const before = s.agent!.reportedAt
    expect(restartDone(s, before, 3000)).toBe(false) // the same report
    const back = server()
    back.agent = { ...back.agent!, reportedAt: '2026-10-11T12:00:04Z', telemt: { ...back.agent!.telemt, uptimeSec: 2 } }
    expect(restartDone(back, before, 3000)).toBe(true)
    // running, but since long before the click: not restarted yet
    const old = server()
    old.agent = { ...old.agent!, reportedAt: '2026-10-11T12:00:04Z' }
    expect(restartDone(old, before, 3000)).toBe(false)
  })
})

describe('logs', () => {
  it('splits a line into time, source and message', () => {
    expect(parseLogLine('2026-10-11T09:32:41Z telemt: cannot bind 0.0.0.0:443: address already in use')).toEqual({
      at: '2026-10-11T09:32:41Z',
      src: 'telemt',
      msg: 'cannot bind 0.0.0.0:443: address already in use',
      tone: 'error',
    })
    expect(parseLogLine('the capture rules are gone')).toMatchObject({ at: null, src: '', tone: '' })
    expect(parseLogLine('2026-10-11T09:34:15.123456Z  WARN telemt keeps crashing').tone).toBe('warn')
    expect(parseLogLine('agent: telemt keeps crashing; waiting 60 s').tone).toBe('warn')
  })

  it('names why the proxy keeps crashing from the newest error that says why', () => {
    const lines = [
      '2026-10-11T09:31:54Z telemt: Telemt MTProxy v3.4.20',
      '2026-10-11T09:32:41Z telemt: cannot bind 0.0.0.0:443: address already in use',
      '2026-10-11T09:33:28Z agent: telemt exited (exit status 1), restart 11',
      '2026-10-11T09:34:15Z agent: telemt keeps crashing; waiting 60 s',
    ]
    expect(crashCause(lines)).toEqual({ text: 'Port 443 is taken by another program on the server.', at: '2026-10-11T09:32:41Z' })
    expect(crashCause(['config error: unknown key "tls"'])).toEqual({ text: 'Config error: unknown key "tls".', at: null })
    expect(crashCause(['agent: telemt exited (exit status 1), restart 3'])).toMatchObject({ text: 'Telemt exited (exit status 1), restart 3.' })
    expect(crashCause(['all good'])).toBeNull()
  })
})

describe('jobs', () => {
  it('names job steps for people, by job kind', () => {
    expect(stepLabel('disguise')).toBe('Check the cover site')
    expect(stepLabel('link', 'install')).toBe('Connect to the agent')
    expect(stepLabel('verify', 'install')).toBe('Check the downloads')
    expect(stepLabel('verify', 'rotate')).toBe('Reconnect with the new keys')
    expect(stepLabel('upload', 'update-agent')).toBe('Send the new version')
    expect(stepLabel('download', 'update-telemt')).toBe('Download the proxy')
    expect(stepLabel('switch', 'link-port')).toBe('Move the link over')
    expect(stepLabel('something-new')).toBe('Something new')
  })

  it('titles a job by kind and state', () => {
    expect(jobTitle({ kind: 'rotate', state: 'running' })).toBe('Renewing link keys')
    expect(jobTitle({ kind: 'update-telemt', state: 'failed' })).toBe('Proxy update failed')
    expect(jobTitle({ kind: 'link-port', state: 'ok' })).toBe('Link port changed')
  })

  it('counts steps, pending ones included', () => {
    const steps = [
      { name: 'download', state: 'ok' as const },
      { name: 'check', state: 'running' as const },
      { name: 'verify', state: 'pending' as const },
    ]
    expect(jobStep(job({ steps }))).toEqual({ n: 2, m: 3 })
    expect(jobStep(job({ steps: steps.map((s) => ({ ...s, state: 'ok' as const })) }))).toEqual({ n: 3, m: 3 })
    expect(jobStep(job({ steps: [] }))).toEqual({ n: 0, m: 0 })
  })

  it('reads a progress percentage from a step detail', () => {
    expect(stepPercent('38% · 3.9 MB sent · about 2 min left')).toBe(38)
    expect(stepPercent('100%')).toBe(100)
    expect(stepPercent('agent 0.5.0')).toBeNull()
    expect(stepPercent(undefined)).toBeNull()
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
    expect(eventLabel('allowlist_violation')).toBe('Refused a non-Telegram address')
    expect(eventLabel('new_kind')).toBe('new kind')
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
