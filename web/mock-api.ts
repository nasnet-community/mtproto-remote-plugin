// A fake plugin API for `npm run dev:mock`: the whole UI runs with sample data
// and no router, VPS or agent. Development only; never built into the page.
//
// Sample servers cover each link state: Frankfurt is up, Helsinki is degraded
// with a crash-looping proxy (port 443 taken; agent and proxy updates due; its
// agent 0.4.2 can't say who is online, so usage.live is false until "Update
// agent"), Amsterdam's link is down (removing it needs a login or force; its
// proxy update is due). Usage moves every few seconds: who is online, their devices
// and traffic, today and the last 30 days, and a 15-second traffic sample per
// server for the live chart. The router is at its cap of 3 servers; remove one
// to try adding. One finished job (Frankfurt's link keys) sits in the job tray.
//
// Add-server scenarios, picked by the address or cover site you type:
//   address contains "fail"    -> fails at "Check the server" (no systemd)
//   address contains "nolink"  -> fails at the link step (not in the domestic list), naming the link port
//   address contains "nft"     -> needs nftables, a step that appears when it starts
//   address contains "offline" -> the host key check cannot connect
//   address contains "changed" -> the host key differs from last time
//   address contains "poll"    -> the job stream is unavailable, so the UI polls
//   cover site contains "bad"  -> the cover-site check fails
//   an existing address        -> refused: it is already a server here (repair it instead)
// Every install lists all its steps from the start (pending) and the upload
// step reports its progress ("38% · 3.9 MB sent · about 2 min left").
//
// Other scenarios: link port 9999 -> the port change fails and moves back.
// A cover name containing "bad" fails its check. An existing secret that another
// user has is refused (409).
//
// Helsinki's cover site is not checked yet (its agent just restarted). The e2e
// reset takes { maxServers, allowlistUrl, slowRemove } to set up other cases, and
// { rebuilt } (Amsterdam presents a new host key), { halfInstalled } (Amsterdam's
// install died before it was saved, so it can only be removed), { oldAgent }
// (Frankfurt runs agent 0.4.2 too, so its users show dashes for online and devices), { allowlistFails } (refreshing the allowlist fails with a 502 that
// carries the list) and { restartHangs } (a restarted proxy never comes back).
// Every other call needs the page's X-MTProto-UI header, as the plugin does.
import { readFileSync } from 'node:fs'
import type { IncomingMessage, ServerResponse } from 'node:http'
import type { Plugin } from 'vite'
import type { AgentState, Allowlist, CoverCheck, Job, JobStep, LinkConn, LinkState, ProxyOptions, Server, ServerEvent, User } from './src/lib/api'

const DAY = 86400e3
const H = 3600e3
const MIN = 60e3
const iso = (ms: number) => new Date(ms).toISOString().replace(/\.\d+Z$/, 'Z')
const ago = (sec: number) => iso(Date.now() - sec * 1000)
const inDays = (d: number) => iso(Date.now() + d * DAY)
const endOfDay = (ms: number) => `${new Date(ms).toISOString().slice(0, 10)}T23:59:59Z`
const hex = (n: number) => Array.from({ length: n }, () => Math.floor(Math.random() * 16).toString(16)).join('')
const hexOf = (s: string) => Buffer.from(s).toString('hex')
const rand = (lo: number, hi: number) => lo + Math.floor(Math.random() * (hi - lo + 1))
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

// What an update installs: the versions the plugin pins.
const AGENT = '0.5.0'
const TELEMT = '3.4.25'
const GB = 1e9
const MB = 1e6
const BUDGET = 6000
const DEFAULT_STREAMS = 2000
const STEP = 15e3
const NEW_KEY = 'SHA256:u3Rq0Q1uV3Jc0mYw7b6o8xk3j2m9Pq5c1f0aZyXwVb8'
const NAME = /^[A-Za-z0-9_.-]{1,32}$/
const DOMAIN = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$/

interface MockUser {
  name: string
  secret: string
  enabled: boolean
  maxIps: number
  expiresAt: string | null
  quotaBytes: number
  up: number
  down: number
  createdAt: string
  note: string
  /** How busy the user is; drives the moving numbers. Online while above 0 and the proxy runs. */
  activity: number
  /** Distinct addresses while online. */
  devs: number
  conns: number
  lastSeen: number | null
  /** Bytes today and in the last 30 days, both ways. */
  today: number
  month: number
}

interface MockServer {
  id: number
  name: string
  host: string
  sshPort: number
  sshUser: string
  hostKey: string
  /** The key the box shows now, when a rebuild changed it. */
  presentedKey?: string
  /** False: the install died before saving, so there is no key, cover site or link. */
  registered: boolean
  createdAt: string
  coverDomain: string
  maxStreams: number
  hubPort: number
  capturePort: number
  arch: string
  agentVersion: string
  telemtVersion: string
  /** The agent is too old to report connections and devices. */
  oldAgent: boolean
  /** How many of the two link connections are up. */
  connsUp: number
  /** When the connections that are down dropped. */
  downAt: number
  lastSeen: number
  agentStart: number
  /** When the cover site was last checked; null: not yet. */
  coverCheckedAt: number | null
  /** start: when it (next) runs; a restart sets it a little ahead. */
  telemt: { running: boolean; restarts: number; start: number; lastExit: string; crashLoop: boolean }
  proxyStopped: boolean
  proxyOptions: ProxyOptions
  stats: { streams: number; relayed: number; dialFailures: number; violations: number; toDc: number; fromDc: number; active: number; rejected: number }
  samples: { at: string; up: number; down: number }[]
  /** Bytes since the last sample, and the rate over the last tick. */
  acc: { up: number; down: number; at: number }
  rate: { up: number; down: number }
  polledAt: number
  users: MockUser[]
  events: ServerEvent[]
}

function user(name: string, o: Partial<MockUser> = {}): MockUser {
  return {
    name,
    secret: hex(32),
    enabled: true,
    maxIps: 0,
    expiresAt: null,
    quotaBytes: 0,
    up: 0,
    down: 0,
    createdAt: ago(86400 * 20),
    note: '',
    activity: 0,
    devs: 1,
    lastSeen: null,
    today: 0,
    month: 0,
    ...o,
    conns: (o.activity ?? 0) * 2 + 1,
  }
}

// Splits a total roughly the way proxy traffic goes: mostly down.
const split = (total: number) => {
  const up = Math.round(total * 0.18)
  return { up, down: Math.round(total) - up }
}
const used = (total: number, month = total, today = 0) => ({ ...split(total), month, today })

function userState(u: MockUser): User['state'] {
  if (!u.enabled) return 'disabled'
  if (u.expiresAt && Date.parse(u.expiresAt) < Date.now()) return 'expired'
  if (u.quotaBytes && u.up + u.down >= u.quotaBytes) return 'over-quota'
  return 'active'
}

const proxyRuns = (s: MockServer) => s.connsUp > 0 && s.telemt.running && !s.proxyStopped && Date.now() >= s.telemt.start
const isOnline = (s: MockServer, u: MockUser) => proxyRuns(s) && u.activity > 0 && userState(u) === 'active'
const live = (s: MockServer) => s.connsUp > 0 && !s.oldAgent

function links(s: MockServer, u: MockUser, domain: string) {
  const q = `server=${s.host}&port=443&secret=ee${u.secret}${hexOf(domain)}`
  return { tg: `tg://proxy?${q}`, https: `https://t.me/proxy?${q}` }
}

function userView(s: MockServer, u: MockUser): User {
  const on = isOnline(s, u)
  const known = live(s)
  const devs = u.maxIps ? Math.min(u.devs, u.maxIps) : u.devs
  const t = split(u.today)
  const m = split(u.month)
  return {
    name: u.name,
    secret: u.secret,
    enabled: u.enabled,
    maxIps: u.maxIps,
    expiresAt: u.expiresAt,
    quotaBytes: u.quotaBytes,
    up: u.up,
    down: u.down,
    createdAt: u.createdAt,
    note: u.note,
    state: userState(u),
    online: known ? on : null,
    connections: known ? (on ? u.conns : 0) : null,
    devices: known ? (on ? devs : 0) : null,
    lastSeenAt: on ? iso(s.polledAt) : u.lastSeen ? iso(u.lastSeen) : null,
    todayUp: t.up,
    todayDown: t.down,
    monthUp: m.up,
    monthDown: m.down,
    links: {
      ...links(s, u, s.coverDomain),
      extra: s.proxyOptions.extraCoverNames.map((domain) => ({ domain, ...links(s, u, domain) })),
    },
  }
}

function linkView(s: MockServer): LinkState {
  const conns: LinkConn[] = [0, 1].map((i) => {
    const up = i < s.connsUp
    return {
      name: `tcp-${i + 1}`,
      up,
      since: up ? iso(s.agentStart) : iso(s.downAt),
      rttMs: up ? 38 + i * 6 : 0,
      streams: up ? Math.round(s.stats.streams / s.connsUp) : 0,
      lastError: up ? '' : `dial tcp ${s.host}:${s.hubPort}: i/o timeout`,
    }
  })
  return {
    state: s.connsUp === 2 ? 'up' : s.connsUp === 1 ? 'degraded' : 'down',
    up: s.connsUp,
    total: 2,
    streams: s.connsUp ? s.stats.streams : 0,
    relayed: s.stats.relayed,
    dialFailures: s.stats.dialFailures,
    violations: s.stats.violations,
    bytesToDc: s.stats.toDc,
    bytesFromDc: s.stats.fromDc,
    conns,
  }
}

function agentView(s: MockServer): AgentState {
  // a down server still shows its last report
  const at = s.connsUp ? Date.now() - 1000 : s.lastSeen
  const runs = s.telemt.running && !s.proxyStopped && at >= s.telemt.start
  return {
    version: s.agentVersion,
    telemtVersion: s.telemtVersion,
    uptimeSec: Math.floor((at - s.agentStart) / 1000),
    rssBytes: 21_400_000 + s.users.length * 40_000,
    stateRev: 'r' + hexOf(s.coverDomain).slice(0, 6),
    reportedAt: iso(at),
    telemt: {
      running: runs,
      restarts: s.telemt.restarts,
      uptimeSec: runs ? Math.floor((at - s.telemt.start) / 1000) : 0,
      lastExit: s.telemt.lastExit,
      crashLoop: s.telemt.crashLoop,
      held: s.proxyStopped ? 'stopped from the router' : '',
    },
    capture: { rules: true, active: s.connsUp ? s.stats.active : 0, rejected: s.stats.rejected },
    cover: s.coverCheckedAt
      ? { domain: s.coverDomain, ok: true, checkedAt: iso(s.coverCheckedAt), error: '' }
      : { domain: s.coverDomain, ok: false, checkedAt: null, error: '' },
  }
}

function usageView(s: MockServer): Server['usage'] {
  const known = live(s)
  const online = known ? s.users.filter((u) => isOnline(s, u)) : []
  const sum = (k: 'today' | 'month') => split(s.users.reduce((n, u) => n + u[k], 0))
  const today = sum('today')
  const month = sum('month')
  return {
    live: known,
    usersOnline: online.length,
    connections: online.reduce((n, u) => n + u.conns, 0),
    devices: online.reduce((n, u) => n + (u.maxIps ? Math.min(u.devs, u.maxIps) : u.devs), 0),
    rateUp: s.connsUp ? Math.round(s.rate.up) : 0,
    rateDown: s.connsUp ? Math.round(s.rate.down) : 0,
    todayUp: today.up,
    todayDown: today.down,
    monthUp: month.up,
    monthDown: month.down,
    updatedAt: s.polledAt ? iso(s.polledAt) : null,
  }
}

function view(s: MockServer, detail = false): Server {
  const rev = 'r' + hexOf(s.coverDomain).slice(0, 6)
  const out: Server = {
    id: s.id,
    name: s.name,
    host: s.host,
    sshPort: s.sshPort,
    sshUser: s.sshUser,
    hostKey: s.hostKey,
    registered: s.registered,
    createdAt: s.createdAt,
    coverDomain: s.coverDomain,
    maxStreams: s.maxStreams,
    hubPort: s.hubPort,
    arch: s.arch,
    agentVersion: s.agentVersion,
    telemtVersion: s.telemtVersion,
    lastSeenAt: s.connsUp ? ago(3) : iso(s.lastSeen),
    userCount: s.users.length,
    link: linkView(s),
    agent: s.registered ? agentView(s) : null,
    sync: {
      desiredRev: rev,
      appliedRev: s.connsUp ? rev : 'r000000',
      lastPushAt: s.connsUp ? ago(40) : iso(s.lastSeen),
      lastPushError: s.connsUp ? '' : 'the link is down',
      healthy: s.connsUp > 0,
    },
    proxyStopped: s.proxyStopped,
    proxyOptions: { ...s.proxyOptions, extraCoverNames: [...s.proxyOptions.extraCoverNames] },
    updates: {
      agent: s.registered && s.agentVersion !== AGENT,
      telemt: s.registered && s.telemtVersion !== TELEMT,
      agentVersion: AGENT,
      telemtVersion: TELEMT,
    },
    usage: usageView(s),
  }
  if (detail) out.events = s.events.slice(0, 50)
  return out
}

const event = (s: MockServer, kind: string, detail = '', at = Date.now()) => {
  s.events.unshift({ at: iso(at), kind, detail })
  s.events.sort((a, b) => Date.parse(b.at) - Date.parse(a.at))
}

/** Fifteen minutes of samples around a typical rate; nothing after `stopAt`. */
function history(down: number, stopAt = Infinity) {
  const now = Date.now()
  return Array.from({ length: 60 }, (_, i) => {
    const at = now - (59 - i) * STEP
    const wave = 1 + 0.35 * Math.sin(i / 4) + (Math.random() - 0.5) * 0.3
    const d = at < stopAt ? Math.round(down * wave) : 0
    return { at: iso(at), up: Math.round(d * 0.22), down: d }
  })
}

const base = () => ({
  presentedKey: undefined,
  oldAgent: false,
  proxyStopped: false,
  proxyOptions: { extraCoverNames: [], deviceWindowSecs: 0, logLevel: 'normal' as const },
  capturePort: 9443,
  acc: { up: 0, down: 0, at: Date.now() },
  rate: { up: 0, down: 0 },
  polledAt: Date.now() - 10e3,
})

let servers: MockServer[] = []
let nextId = 1
let maxServers = 3
let allowlist: Allowlist
/** Removal jobs wait this long after forgetting the server, like a slow last event. */
let removeTail = 0
let allowlistFails = false
let restartHangs = false

function seed() {
  const now = Date.now()
  const fra: MockServer = {
    ...base(),
    id: 1, name: 'Frankfurt VPS', host: '203.0.113.10', sshPort: 22, sshUser: 'root', hostKey: 'SHA256:p2OiwWbYoEAsmmYWEMy1L12ya7lUDzuHYODmtJ18R/w', registered: true,
    createdAt: ago(86400 * 41), coverDomain: 'www.example.com', maxStreams: 2000, hubPort: 8443, arch: 'amd64', agentVersion: AGENT, telemtVersion: TELEMT,
    connsUp: 2, downAt: 0, lastSeen: now, agentStart: now - 6 * DAY - 5 * H, coverCheckedAt: now - H,
    telemt: { running: true, restarts: 0, start: now - 6 * DAY - 5 * H, lastExit: '', crashLoop: false },
    stats: { streams: 412, relayed: 1_884_201, dialFailures: 3, violations: 0, toDc: 412 * GB, fromDc: 1906 * GB, active: 388, rejected: 2 },
    samples: history(1.2 * MB),
    users: [
      user('alice', { note: "Mom's tablet", activity: 2, devs: 2, ...used(3.9 * GB, 3.1 * GB, 120 * MB), quotaBytes: 50 * GB, maxIps: 3, expiresAt: inDays(21), lastSeen: now }),
      user('bob', { note: 'Laptop', enabled: false, ...used(0.91 * GB), lastSeen: now - 2 * DAY }),
      user('family', { note: 'Living room TV and phones', activity: 3, devs: 5, ...used(148.3 * GB, 41 * GB, 1.4 * GB), maxIps: 8, lastSeen: now }),
      user('office', { note: 'Shared office Wi-Fi', activity: 3, devs: 11, ...used(612.4 * GB, 96 * GB, 2.1 * GB), lastSeen: now }),
      user('dad_laptop', { note: 'Dad', activity: 1, devs: 1, ...used(21.6 * GB, 21.6 * GB, 310 * MB), quotaBytes: 100 * GB, maxIps: 2, expiresAt: inDays(0.2), lastSeen: now }),
      user('carol', { note: 'Carol, upstairs', ...used(78.2 * GB, 31 * GB, 0.9 * GB), quotaBytes: 100 * GB, maxIps: 2, expiresAt: inDays(81), lastSeen: now - 12 * MIN }),
      user('grace', { note: "Grace's phone", ...used(84 * GB, 12.1 * GB), quotaBytes: 100 * GB, maxIps: 2, lastSeen: now - 3 * H }),
      user('trial-7d', { note: 'Week trial for Sam', ...used(4.6 * GB), quotaBytes: 5 * GB, maxIps: 1, expiresAt: endOfDay(now - 6 * DAY), lastSeen: now - 6 * DAY }),
      user('used-up', { note: 'Neighbour', ...used(10 * GB), quotaBytes: 10 * GB, lastSeen: now - H }),
      user('guest-wifi', { note: 'Guests', enabled: false }),
      user('kids-ipad', { note: "Kids' iPad", activity: 1, devs: 1, ...used(7.2 * GB, 2.3 * GB, 150 * MB), lastSeen: now }),
      user('a-long-user-name.with.dots_2026', { ...used(0.05 * GB), expiresAt: inDays(365), lastSeen: now - 5 * MIN }),
      user('dave', { activity: 1, devs: 1, ...used(2.4 * GB, 1.1 * GB, 40 * MB), maxIps: 2, lastSeen: now }),
      user('erin', { ...used(18 * GB, 6 * GB), quotaBytes: 100 * GB, lastSeen: now - 26 * H }),
      user('frank', { ...used(0.4 * GB), maxIps: 2, lastSeen: now - 4 * DAY }),
      user('heidi', { ...used(1.9 * GB, 1.9 * GB, 20 * MB), expiresAt: inDays(4), lastSeen: now - 40 * MIN }),
      user('ivan', { ...used(33 * GB, 8 * GB), lastSeen: now - 7 * H }),
      user('judy', { enabled: false, ...used(5 * GB, 0.2 * GB), lastSeen: now - 9 * DAY }),
      user('mallory', { ...used(0.7 * GB), maxIps: 2, expiresAt: inDays(120), lastSeen: now - 2 * H }),
      user('nina', { ...used(12 * GB, 3 * GB), quotaBytes: 100 * GB, lastSeen: now - 20 * MIN }),
      user('oscar', { ...used(0.1 * GB), lastSeen: now - 12 * DAY }),
      user('peggy', { ...used(3.3 * GB, 2.2 * GB, 10 * MB), maxIps: 2, lastSeen: now - 50 * MIN }),
    ],
    events: [],
  }
  event(fra, 'link_up', '2 of 2 connections', fra.agentStart)
  event(fra, 'telemt_up', `telemt ${TELEMT}`, fra.agentStart + 2000)
  event(fra, 'dc_dial_failed', 'dial 192.0.2.80:443: i/o timeout', now - 2 * H)

  const crashedAt = now - 6 * MIN
  const hel: MockServer = {
    ...base(),
    oldAgent: true,
    id: 2, name: 'Helsinki ARM', host: 'hel.example.net', sshPort: 2222, sshUser: 'deploy', hostKey: 'SHA256:FkImfecZh+m0r8bQdJoD77DxMU9mViXNaqX/kA9ZISU', registered: true,
    createdAt: ago(86400 * 9), coverDomain: 'static.example.net', maxStreams: 1500, hubPort: 8443, arch: 'arm64', agentVersion: '0.4.2', telemtVersion: '3.4.20',
    connsUp: 1, downAt: now - 2 * H, lastSeen: now, agentStart: now - 20 * H - 3 * MIN, coverCheckedAt: null,
    telemt: { running: false, restarts: 14, start: now - 20 * H, lastExit: 'exit status 1', crashLoop: true },
    stats: { streams: 0, relayed: 20_310, dialFailures: 0, violations: 3, toDc: 4.1 * GB, fromDc: 22 * GB, active: 0, rejected: 7 },
    samples: history(1.3 * MB, crashedAt),
    users: [
      user('user1', { activity: 1, ...used(2.1 * GB, 2.1 * GB, 160 * MB), lastSeen: crashedAt }),
      user('team-a', { activity: 2, devs: 4, ...used(55 * GB, 20 * GB, 120 * MB), maxIps: 10, lastSeen: crashedAt }),
      user('team-b', { ...used(18 * GB, 9 * GB, 30 * MB), maxIps: 10, lastSeen: now - 3 * H }),
      user('backup-link', { enabled: false }),
      user('conference', { ...used(0.3 * GB), expiresAt: inDays(1.5), lastSeen: now - DAY }),
    ],
    events: [],
  }
  event(hel, 'link_up', '2 of 2 connections', hel.agentStart)
  event(hel, 'allowlist_violation', '3 tries in 5 minutes, last to 198.51.100.9:443', now - 3 * H)
  event(hel, 'link_down', 'tcp-2: i/o timeout', now - 2 * H)
  event(hel, 'telemt_down', 'exit status 1', crashedAt)
  event(hel, 'telemt_crash_loop', '14 restarts; waiting longer between tries', now - 4 * MIN)

  const ams: MockServer = {
    ...base(),
    id: 3, name: 'Amsterdam backup', host: '198.51.100.77', sshPort: 22, sshUser: 'root', hostKey: 'SHA256:3c4Q9zqk0d1s0l2mUu5l0cT8m2o9aXq7r6c4e1YwZ0A', registered: true,
    createdAt: ago(86400 * 120), coverDomain: 'www.example.org', maxStreams: 1000, hubPort: 8443, arch: 'amd64', agentVersion: AGENT, telemtVersion: '3.4.24',
    connsUp: 0, downAt: now - 3 * H, lastSeen: now - 3 * H, agentStart: now - 2 * DAY, coverCheckedAt: now - 4 * H,
    telemt: { running: true, restarts: 0, start: now - 2 * DAY, lastExit: '', crashLoop: false },
    stats: { streams: 0, relayed: 99_120, dialFailures: 0, violations: 0, toDc: 9 * GB, fromDc: 40 * GB, active: 0, rejected: 0 },
    samples: [],
    polledAt: now - 3 * H,
    users: [
      user('spare1', { note: 'Backup for the family', ...used(12 * GB), lastSeen: now - 3 * H }),
      user('spare2', { note: 'Not handed out yet' }),
    ],
    events: [],
  }
  event(ams, 'link_up', '2 of 2 connections', ams.agentStart)
  event(ams, 'link_down', 'both connections: i/o timeout', ams.lastSeen)

  servers = [fra, hel, ams]
  nextId = 4
  maxServers = 3
  removeTail = 0
  allowlistFails = false
  restartHangs = false
  allowlist = { prefixes: 1, placeholder: true, refreshedAt: null, lastError: '', url: 'https://lists.example.net/allowlist.txt' }
  jobs.clear()
  installing.clear()
  // what the tray shows from before: Frankfurt's link keys, renewed 25 minutes ago
  const done = now - 25 * MIN
  jobs.set('seed-rotate', {
    view: {
      id: 'seed-rotate', kind: 'rotate', state: 'ok', serverId: 1, target: '', startedAt: iso(done - 41e3), finishedAt: iso(done),
      steps: [{ name: 'rotate', state: 'ok' }, { name: 'verify', state: 'ok' }],
    },
    listeners: new Set(),
    noStream: false,
  })
}

// Moving numbers. Started by the dev server only, so importing this file (as
// vite.config.ts does) never keeps a `vite build` process alive.
function tick() {
  const now = Date.now()
  for (const s of servers) {
    const runs = proxyRuns(s)
    let up = 0
    let down = 0
    if (runs) {
      s.stats.streams = Math.max(0, s.stats.streams + rand(-20, 20))
      s.stats.active = Math.max(0, s.stats.streams - rand(0, 30))
      s.stats.relayed += rand(5, 60)
      for (const u of s.users) {
        if (!isOnline(s, u)) continue
        u.conns = u.activity * 2 + rand(0, 2)
        u.lastSeen = now
        const bytes = rand(20_000, 400_000) * u.activity * 4
        const p = split(bytes)
        u.up += p.up
        u.down += p.down
        u.today += bytes
        u.month += bytes
        up += p.up
        down += p.down
      }
      s.stats.toDc += up
      s.stats.fromDc += down
    }
    s.rate = { up: up / 2.5, down: down / 2.5 }
    s.acc.up += up
    s.acc.down += down
    // the plugin reads usage every 15 s while the link is up
    if (s.connsUp && now - s.acc.at >= STEP) {
      const secs = (now - s.acc.at) / 1000
      s.samples.push({ at: iso(now), up: Math.round(s.acc.up / secs), down: Math.round(s.acc.down / secs) })
      s.samples = s.samples.slice(-60)
      s.acc = { up: 0, down: 0, at: now }
      s.polledAt = now
    }
  }
}

// --- jobs ---

interface MockJob {
  view: Job
  listeners: Set<(j: Job) => void>
  /** The job's event stream answers 503, so the page has to poll. */
  noStream: boolean
}
const jobs = new Map<string, MockJob>()
/** Hosts with an install still running. */
const installing = new Set<string>()

interface Step {
  name: string
  detail?: string
  ms?: number
  /** Fails the step and the job with this message. */
  fail?: string | false
  /** Runs when the step succeeds. */
  then?: (j: MockJob) => void
  /** Reports its progress in the detail, about every 300 ms, like an upload. */
  progress?: boolean
  /** Not in the job's plan: it appears in place when it starts. */
  unplanned?: boolean
}

const snapshot = (j: MockJob): Job => ({ ...j.view, steps: j.view.steps.map((s) => ({ ...s })) })
const emit = (j: MockJob) => j.listeners.forEach((l) => l(snapshot(j)))
const busy = (serverId: number) => [...jobs.values()].some((j) => j.view.state === 'running' && j.view.serverId === serverId)

function startJob(
  kind: Job['kind'],
  steps: Step[],
  o: { serverId?: number; target?: string; noStream?: boolean; tail?: number; done?: () => void; end?: () => void } = {},
) {
  const j: MockJob = {
    view: {
      id: hex(16),
      kind,
      state: 'running',
      // every planned step is listed from the start
      steps: steps.filter((s) => !s.unplanned).map((s) => ({ name: s.name, state: 'pending' as const })),
      serverId: o.serverId ?? 0,
      target: o.target ?? '',
      startedAt: iso(Date.now()),
    },
    listeners: new Set(),
    noStream: !!o.noStream,
  }
  jobs.set(j.view.id, j)
  const finish = (error?: string) => {
    o.end?.()
    j.view.state = error ? 'failed' : 'ok'
    if (error) j.view.error = error
    j.view.finishedAt = iso(Date.now())
    emit(j)
  }
  void (async () => {
    await sleep(300)
    for (const step of steps) {
      let s: JobStep | undefined = j.view.steps.find((x) => x.name === step.name && x.state === 'pending')
      if (!s) {
        s = { name: step.name, state: 'pending' }
        const at = j.view.steps.findIndex((x) => x.state === 'pending')
        j.view.steps.splice(at < 0 ? j.view.steps.length : at, 0, s)
      }
      s.state = 'running'
      emit(j)
      const ms = step.ms ?? 380
      if (step.progress) {
        for (let p = 8; p < 100; p += 15) {
          s.detail = `${p}% · ${((p / 100) * 10.3).toFixed(1)} MB sent · ${p < 70 ? `about ${Math.ceil((100 - p) / 35)} min` : 'less than a minute'} left`
          emit(j)
          await sleep(ms / 7)
        }
      } else await sleep(ms)
      if (step.fail) {
        Object.assign(s, { state: 'failed', detail: step.fail })
        return finish(step.fail)
      }
      Object.assign(s, { state: 'ok', detail: step.detail ?? (step.progress ? '10.3 MB sent' : undefined) })
      step.then?.(j)
      emit(j)
    }
    o.done?.()
    if (o.tail) await sleep(o.tail)
    finish()
  })()
  return j.view.id
}

function streamJob(req: IncomingMessage, res: ServerResponse, j: MockJob) {
  res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache', Connection: 'keep-alive' })
  const write = (v: Job) => {
    res.write(`data: ${JSON.stringify(v)}\n\n`)
    if (v.state !== 'running') {
      j.listeners.delete(write)
      res.end()
    }
  }
  j.listeners.add(write)
  req.on('close', () => j.listeners.delete(write))
  write(snapshot(j))
}

// --- checks ---

function coverCheck(domain: string): CoverCheck {
  if (domain.includes('bad'))
    return { ok: false, tls13: false, x25519: false, h2: true, certOk: true, addrs: ['192.0.2.44'], latencyMs: 61, error: `${domain} does not offer TLS 1.3` }
  if (domain.includes('nowhere'))
    return { ok: false, tls13: false, x25519: false, h2: false, certOk: false, addrs: [], latencyMs: 0, error: `no such host: ${domain}` }
  return { ok: true, tls13: true, x25519: true, h2: true, certOk: true, addrs: ['192.0.2.10', '2001:db8::10'], latencyMs: 38, error: '' }
}

const allocated = (except?: MockServer) => servers.reduce((n, s) => (s === except ? n : n + s.maxStreams), 0)

// The installer refuses any key but the one the box shows.
function wrongKey(s: MockServer, b: Record<string, unknown>) {
  const shown = s.presentedKey ?? (s.hostKey || NEW_KEY)
  return b.hostKey === shown ? false : `the server's host key is ${shown}, expected ${b.hostKey}`
}

function badLogin(b: Record<string, unknown>) {
  if (!b.password && !b.privateKey) return 'give a password or a private key'
  if (!String(b.hostKey ?? '').startsWith('SHA256:')) return "confirm the server's host key first"
  return ''
}

const badPort = (p: number) => !Number.isInteger(p) || p < 1024 || p > 65535 || p === 22

// SSH-only steps of a reinstall; repair runs them over a kept configuration.
const reinstall = (s: MockServer): Step[] => [
  { name: 'check', detail: `${s.arch} Ubuntu 24.04` },
  { name: 'verify' },
  { name: 'account', detail: 'uid 998' },
  { name: 'upload', ms: 1400, progress: true },
  { name: 'directories' },
  { name: 'disguise', detail: s.coverDomain },
  { name: 'credentials', detail: 'kept' },
  { name: 'service' },
]

function bringUp(s: MockServer) {
  s.connsUp = 2
  s.lastSeen = Date.now()
  s.agentStart = Date.now()
  s.polledAt = Date.now()
  s.acc = { up: 0, down: 0, at: Date.now() }
  s.telemt = { running: true, restarts: 0, start: Date.now(), lastExit: '', crashLoop: false }
  event(s, 'link_up', '2 of 2 connections')
}

function install(b: Record<string, unknown>) {
  const host = String(b.host)
  const cover = String(b.coverDomain)
  const name = String(b.name || host)
  const first = String(b.firstUser ?? '')
  const hubPort = Number(b.hubPort) || 8443
  let s: MockServer | undefined
  installing.add(host)
  return startJob(
    'install',
    [
      // tested before the job started (see POST servers)
      { name: 'disguise', detail: cover, ms: 0 },
      { name: 'download', detail: `agent ${AGENT}, telemt ${TELEMT}`, ms: 700 },
      { name: 'check', detail: 'amd64 Ubuntu 24.04', fail: host.includes('fail') && 'this server does not run systemd' },
      ...(host.includes('nft') ? [{ name: 'nftables', detail: 'installed nftables 1.0.9', ms: 600, unplanned: true }] : []),
      { name: 'verify' },
      { name: 'account', detail: 'uid 998' },
      { name: 'upload', ms: 2100, progress: true },
      { name: 'directories' },
      { name: 'credentials' },
      { name: 'service' },
      {
        name: 'register',
        detail: name,
        then: (j) => {
          s = {
            ...base(),
            id: nextId++, name, host, sshPort: Number(b.sshPort) || 22, sshUser: String(b.sshUser || 'root'), hostKey: String(b.hostKey), registered: true,
            createdAt: iso(Date.now()), coverDomain: cover, maxStreams: Number(b.maxStreams) || DEFAULT_STREAMS, hubPort, capturePort: Number(b.capturePort) || 9443, arch: 'amd64',
            agentVersion: AGENT, telemtVersion: TELEMT, connsUp: 0, downAt: Date.now(), lastSeen: Date.now(), agentStart: Date.now(), coverCheckedAt: Date.now(),
            telemt: { running: true, restarts: 0, start: Date.now(), lastExit: '', crashLoop: false },
            stats: { streams: 0, relayed: 0, dialFailures: 0, violations: 0, toDc: 0, fromDc: 0, active: 0, rejected: 0 },
            samples: [],
            users: first ? [user(first, { createdAt: iso(Date.now()) })] : [],
            events: [],
          }
          servers.push(s)
          j.view.serverId = s.id
        },
      },
      {
        name: 'link',
        ms: 700,
        fail: host.includes('nolink') && `the agent is running but the router cannot reach it on TCP port ${hubPort}`,
        then: () => s && bringUp(s),
      },
    ],
    { noStream: host.includes('poll'), target: name, end: () => installing.delete(host) },
  )
}

const LEVELS = ['silent', 'normal', 'verbose', 'debug']

/** Checks proxy options as the plugin does; returns an error or ''. */
function badOptions(s: MockServer, o: Record<string, unknown>) {
  if (o.extraCoverNames !== undefined) {
    const names = o.extraCoverNames
    if (!Array.isArray(names)) return 'extraCoverNames must be a list'
    if (names.length > 4) return 'at most 4 extra cover names'
    for (const n of names) {
      if (typeof n !== 'string' || !DOMAIN.test(n)) return `${n} is not a domain name`
      if (n === s.coverDomain) return `${n} is the main cover site`
    }
    if (new Set(names).size !== names.length) return 'a cover name is listed twice'
  }
  if (o.deviceWindowSecs !== undefined) {
    const w = Number(o.deviceWindowSecs)
    if (!Number.isInteger(w) || w < 0 || w > 86400) return 'the device window must be 0 to 86400 seconds'
  }
  if (o.logLevel !== undefined && !LEVELS.includes(String(o.logLevel))) return `unknown log level ${o.logLevel}`
  return ''
}

// --- the middleware ---

export function mockApi(): Plugin {
  return {
    name: 'mock-api',
    configureServer(server) {
      const ticker = setInterval(tick, 2500)
      server.httpServer?.on('close', () => clearInterval(ticker))
      // Test hook: the page as the panel opens it, without a trailing slash and with a <base>.
      server.middlewares.use(async (req, res, next) => {
        if (req.url?.split(/[?#]/)[0] !== '/__panel-view') return next()
        const html = await server.transformIndexHtml(req.url, readFileSync('index.html', 'utf8'))
        res.setHeader('Content-Type', 'text/html')
        res.end(html.replace('<head>', '<head><base href="/">'))
      })
      server.middlewares.use('/api', async (req, res) => {
        const chunks: Buffer[] = []
        for await (const c of req) chunks.push(c as Buffer)
        const body: Record<string, any> = chunks.length ? JSON.parse(Buffer.concat(chunks).toString() || '{}') : {}
        const method = req.method ?? 'GET'
        const [rawPath, query = ''] = (req.url ?? '').split('?')
        const path = rawPath.replace(/^\//, '')
        const send = (status: number, data: unknown) => {
          res.statusCode = status
          res.setHeader('Content-Type', 'application/json')
          res.end(JSON.stringify(data))
        }
        const fail = (status: number, error: string, extra: object = {}) => send(status, { error, ...extra })
        const started = (jobId: string) => send(202, { jobId })
        await sleep(rand(80, 220))

        // Like the plugin: only the page's own calls get in (the job stream can't send headers).
        const stream = method === 'GET' && /^jobs\/[^/]+\/stream$/.test(path)
        if (path !== '__reset' && !stream && req.headers['x-mtproto-ui'] !== '1') return fail(403, 'requests from other sites are refused')

        // Test hook: back to the sample data, optionally with a different cap.
        if (path === '__reset' && method === 'POST') {
          seed()
          if (body.maxServers) maxServers = Number(body.maxServers)
          if (typeof body.allowlistUrl === 'string') allowlist.url = body.allowlistUrl
          if (body.slowRemove) removeTail = 6000
          if (body.allowlistFails) allowlistFails = true
          if (body.restartHangs) restartHangs = true
          if (body.noJobs) jobs.clear()
          const [fra, , ams] = servers
          if (body.oldAgent) Object.assign(fra, { oldAgent: true, agentVersion: '0.4.2' })
          if (body.rebuilt) ams.presentedKey = NEW_KEY
          // what the plugin keeps of an install that died between register and save
          if (body.halfInstalled)
            Object.assign(ams, { registered: false, hostKey: '', sshPort: 0, sshUser: '', coverDomain: '', hubPort: 0, arch: '', agentVersion: '', telemtVersion: '', users: [], events: [] })
          return send(200, { ok: true })
        }
        if (path === 'config' && method === 'GET')
          return send(200, {
            version: AGENT,
            maxServers,
            serverCount: servers.length,
            streamBudget: BUDGET,
            streamsAllocated: allocated(),
            defaultMaxStreams: DEFAULT_STREAMS,
            allowlist,
          })
        if (path === 'allowlist/refresh' && method === 'POST') {
          if (!allowlist.url) return fail(409, 'no refresh URL is configured')
          await sleep(500)
          if (allowlistFails) {
            allowlist = { ...allowlist, refreshedAt: iso(Date.now()), lastError: 'lists.example.net did not answer within 20 s' }
            return fail(502, `the list could not be refreshed: ${allowlist.lastError}`, { allowlist })
          }
          allowlist = { ...allowlist, prefixes: 12, refreshedAt: iso(Date.now()), lastError: '' }
          return send(200, allowlist)
        }
        if (path === 'probe' && method === 'POST') {
          const host = String(body.host)
          if (host.includes('offline')) return fail(502, `could not connect to ${host}:${body.sshPort}: connection refused`)
          const here = servers.find((s) => s.host === host)
          // like the plugin: only a pinned key makes a host known
          const pinned = here?.hostKey || ''
          const shown = here?.presentedKey ?? (pinned || NEW_KEY)
          return send(200, {
            fingerprint: shown,
            status: host.includes('changed') || (pinned && shown !== pinned) ? 'changed' : pinned ? 'known' : 'new',
          })
        }

        if (path === 'jobs' && method === 'GET') {
          const hour = Date.now() - H
          const list = [...jobs.values()]
            .map((j) => j.view)
            .filter((v) => v.state === 'running' || Date.parse(v.finishedAt ?? v.startedAt) > hour)
            .sort((a, b) => Date.parse(b.startedAt) - Date.parse(a.startedAt))
            .slice(0, 20)
          return send(200, list.map((v) => snapshot({ view: v, listeners: new Set(), noStream: false })))
        }
        const jm = path.match(/^jobs\/([^/]+)(\/stream)?$/)
        if (jm && method === 'GET') {
          const j = jobs.get(decodeURIComponent(jm[1]))
          if (!j) return fail(404, 'no such job')
          if (!jm[2]) return send(200, snapshot(j))
          if (j.noStream) return fail(503, 'job streams are not available here')
          return streamJob(req, res, j)
        }

        if (path === 'servers' && method === 'GET') return send(200, servers.map((s) => view(s)))
        if (path === 'servers' && method === 'POST') {
          if (servers.length >= maxServers) return fail(409, `this router runs at most ${maxServers} servers`)
          const host = String(body.host ?? '').trim()
          if (servers.some((x) => x.host === host)) return fail(409, `${host} is already a server here: use repair on its page`)
          if (installing.has(host)) return fail(409, `an install to ${host} is still running`)
          if (!host) return fail(400, 'give the server address')
          if (!DOMAIN.test(String(body.coverDomain ?? ''))) return fail(400, 'give the cover site as a domain name')
          const login = badLogin(body)
          if (login) return fail(400, login)
          if (body.maxStreams !== undefined && (!Number.isInteger(body.maxStreams) || body.maxStreams < 100))
            return fail(400, 'the connection limit must be at least 100')
          const hub = body.hubPort === undefined ? 8443 : Number(body.hubPort)
          const cap = body.capturePort === undefined ? 9443 : Number(body.capturePort)
          if (badPort(hub) || badPort(cap) || hub === cap) return fail(400, 'the link and capture ports must be 1024 to 65535, not 22, and differ')
          const left = BUDGET - allocated()
          if ((Number(body.maxStreams) || DEFAULT_STREAMS) > left) return fail(409, `only ${left} connections are left on this router`)
          // like the plugin: the login and the cover site are tested before the job starts
          await sleep(900)
          if (body.password === 'wrong') return fail(502, 'ssh: unable to authenticate: the password or key was not accepted')
          const cover = String(body.coverDomain).trim().toLowerCase()
          const check = coverCheck(cover)
          if (!check.ok) return fail(422, `${cover} does not work as a cover site from this VPS: ${check.error}`, { cover: { ...check, latencyMs: 0 } })
          return started(install(body))
        }

        const m = path.match(/^servers\/(\d+)(?:\/(.*))?$/)
        if (!m) return fail(404, 'not found')
        const s = servers.find((x) => x.id === Number(m[1]))
        if (!s) return fail(404, 'no such server')
        const rest = m[2] ?? ''
        const linkDown = () => (s.connsUp ? '' : 'the link to this server is down')

        if (rest === '' && method === 'GET') return send(200, view(s, true))
        if (rest === '' && method === 'PATCH') {
          const name = String(body.name ?? '').trim()
          if (!name || name.length > 100) return fail(400, 'name must be 1 to 100 characters')
          s.name = name
          return send(200, view(s, true))
        }
        if (rest === 'traffic' && method === 'GET') return send(200, { stepSec: 15, samples: s.samples.slice(-60) })
        if (rest === 'settings' && method === 'PATCH') {
          if (body.maxStreams !== undefined) {
            const n = Number(body.maxStreams)
            if (!Number.isInteger(n) || n < 100 || n > 20000) return fail(400, 'the connection limit must be 100 to 20000')
            const left = BUDGET - allocated(s)
            if (n > left) return fail(409, `only ${left} connections are left on this router`)
          }
          const opts = body.proxyOptions as Record<string, unknown> | undefined
          if (opts) {
            const bad = badOptions(s, opts)
            if (bad) return fail(400, bad)
          }
          if (body.coverDomain !== undefined) {
            if (linkDown()) return fail(503, linkDown())
            const domain = String(body.coverDomain).trim().toLowerCase()
            if (!DOMAIN.test(domain)) return fail(400, 'give the cover site as a domain name')
            const check = coverCheck(domain)
            if (!check.ok) return fail(400, check.error)
            await sleep(400)
            s.coverDomain = domain
            s.coverCheckedAt = Date.now()
          }
          if (body.maxStreams !== undefined) s.maxStreams = Number(body.maxStreams)
          if (opts) {
            const before = JSON.stringify([s.proxyOptions.extraCoverNames, s.proxyOptions.logLevel])
            s.proxyOptions = { ...s.proxyOptions, ...(opts as Partial<ProxyOptions>) }
            // a core option restarts the proxy by itself
            if (before !== JSON.stringify([s.proxyOptions.extraCoverNames, s.proxyOptions.logLevel]) && s.telemt.running)
              s.telemt = { ...s.telemt, start: Date.now() + 1500 }
          }
          return send(200, view(s, true))
        }
        if (rest === 'disguise-check' && method === 'POST') {
          if (linkDown()) return fail(503, linkDown())
          await sleep(600)
          return send(200, coverCheck(String(body.domain ?? '').trim().toLowerCase()))
        }
        if (rest === 'proxy' && method === 'POST') {
          if (typeof body.stopped !== 'boolean') return fail(400, 'stopped must be true or false')
          if (body.stopped && !s.proxyStopped) event(s, 'telemt_down', 'stopped from the router')
          if (!body.stopped && s.proxyStopped) {
            s.telemt = { ...s.telemt, running: true, crashLoop: false, start: Date.now() + 1500 }
            event(s, 'telemt_up', 'started from the router')
          }
          s.proxyStopped = body.stopped
          return send(200, view(s, true))
        }
        if (rest === 'link-port' && method === 'POST') {
          const port = Number(body.port)
          if (badPort(port)) return fail(400, 'the link port must be 1024 to 65535, and not 22')
          if (port === s.hubPort) return fail(400, `the link already uses port ${port}`)
          if (port === s.capturePort) return fail(400, `port ${port} is the capture port`)
          if (port === s.sshPort) return fail(400, `port ${port} is the SSH port`)
          if (linkDown()) return fail(503, linkDown())
          if (busy(s.id)) return fail(409, 'another job is running for this server')
          const old = s.hubPort
          return started(
            startJob(
              'link-port',
              [
                { name: 'open', detail: `port ${port}` },
                {
                  name: 'switch',
                  ms: 900,
                  fail: port === 9999 && `the link did not come up on port ${port}; it is back on ${old}`,
                  then: () => {
                    s.hubPort = port
                    event(s, 'link_up', `2 of 2 connections, on port ${port}`)
                  },
                },
                { name: 'close', detail: `port ${old}` },
              ],
              { serverId: s.id },
            ),
          )
        }
        if (rest === 'repair' && method === 'POST') {
          const login = badLogin(body)
          if (login) return fail(400, login)
          const steps = reinstall(s)
          if (body.password === 'wrong') steps[0].fail = 'ssh: unable to authenticate: the password or key was not accepted'
          else steps[0].fail = wrongKey(s, body)
          // a repair that worked pins the key it logged in with
          steps[0].then = () => ((s.hostKey = String(body.hostKey)), delete s.presentedKey)
          steps.push({ name: 'link', ms: 600, then: () => !s.connsUp && bringUp(s) })
          return started(startJob('repair', steps, { serverId: s.id }))
        }
        if (rest === 'remove' && method === 'POST') {
          const login = body.login as Record<string, unknown> | undefined
          if (!s.connsUp && !login && !body.force)
            return fail(409, 'the router cannot reach this server: log in over SSH, or force the removal', { needLogin: true })
          if (login && badLogin(login)) return fail(400, badLogin(login))
          const steps: Step[] = []
          if (s.connsUp) steps.push({ name: 'remove', detail: 'removed over the link' })
          else if (login) steps.push({ name: 'remove', detail: 'removed over SSH', ms: 700, fail: wrongKey(s, login) })
          steps.push({ name: 'forget', then: () => servers.splice(servers.indexOf(s), 1) })
          return started(startJob('uninstall', steps, { serverId: s.id, tail: removeTail }))
        }
        if (rest === 'update' && method === 'POST') {
          if (body.kind !== 'agent' && body.kind !== 'telemt') return fail(400, 'kind must be agent or telemt')
          if (linkDown()) return fail(503, linkDown())
          if (body.kind === 'agent' ? s.agentVersion === AGENT : s.telemtVersion === TELEMT) return fail(409, 'no update is due')
          if (body.kind === 'agent')
            return started(
              startJob(
                'update-agent',
                [
                  { name: 'download', detail: `agent ${AGENT}` },
                  { name: 'upload', ms: 1100, progress: true },
                  { name: 'restart', ms: 800, then: () => Object.assign(s, { agentVersion: AGENT, agentStart: Date.now(), oldAgent: false }) },
                  { name: 'commit' },
                ],
                { serverId: s.id },
              ),
            )
          return started(
            startJob(
              'update-telemt',
              [
                { name: 'download', detail: `telemt ${TELEMT}` },
                { name: 'upload', ms: 1100, progress: true, then: () => ((s.telemtVersion = TELEMT), (s.telemt.start = Date.now())) },
              ],
              { serverId: s.id },
            ),
          )
        }
        if (rest === 'rotate' && method === 'POST') {
          if (linkDown()) return fail(503, linkDown())
          return started(startJob('rotate', [{ name: 'rotate' }, { name: 'verify', ms: 700 }], { serverId: s.id }))
        }
        if (rest === 'restart' && method === 'POST') {
          if (linkDown()) return fail(503, linkDown())
          await sleep(300)
          // the request returns at once; the proxy is back a few seconds later
          s.telemt = restartHangs
            ? { ...s.telemt, running: false, crashLoop: false }
            : { ...s.telemt, running: true, crashLoop: false, start: Date.now() + 2500 }
          event(s, 'telemt_up', 'restarted from the router', Date.now() + 2500)
          return send(200, { ok: true })
        }
        if (rest === 'logs' && method === 'GET') {
          if (linkDown()) return fail(503, linkDown())
          return send(200, { lines: logs(s, Number(new URLSearchParams(query).get('lines') ?? 300)) })
        }

        // Users live on the router, so they work while the link is down too.
        if (rest === 'users' && method === 'GET') return send(200, s.users.map((u) => userView(s, u)))
        if (rest === 'users' && method === 'POST') {
          const name = String(body.name ?? '')
          if (!NAME.test(name) || /^\.+$/.test(name)) return fail(400, "the name must be 1 to 32 letters, digits, '.', '_' or '-'")
          if (s.users.some((u) => u.name === name)) return fail(409, `a user named ${name} already exists`)
          const note = String(body.note ?? '').trim()
          if (note.length > 80) return fail(400, 'the note must be at most 80 characters')
          let secret = hex(32)
          if (body.secret) {
            const raw = String(body.secret).trim().toLowerCase()
            const sm = /^(?:dd)?([0-9a-f]{32})$/.exec(raw) ?? /^ee([0-9a-f]{32})(?:[0-9a-f]{2})+$/.exec(raw)
            if (!sm) return fail(400, 'the secret must be 32 hex characters, or dd or ee followed by them')
            if (s.users.some((u) => u.secret === sm[1])) return fail(409, 'another user on this server already has this secret')
            secret = sm[1]
          }
          const u = user(name, { createdAt: iso(Date.now()), secret, note })
          apply(u, body)
          s.users.push(u)
          return send(201, userView(s, u))
        }
        if (rest === 'users/bulk' && method === 'POST') {
          const names: string[] = Array.isArray(body.names) ? body.names.map(String) : []
          if (!names.length) return fail(400, 'give at least one user')
          const action = String(body.action)
          if (!['pause', 'resume', 'renew', 'delete'].includes(action)) return fail(400, `unknown action ${action}`)
          const days = body.days === undefined ? 30 : Number(body.days)
          if (action === 'renew' && (!Number.isInteger(days) || days < 1 || days > 3650)) return fail(400, 'days must be 1 to 3650')
          let changed = 0
          for (const u of s.users.filter((x) => names.includes(x.name))) {
            if (action === 'pause') u.enabled = false
            else if (action === 'resume') u.enabled = true
            else if (action === 'renew') {
              // no expiry stays none; expired renews from today, otherwise from the expiry
              if (!u.expiresAt) continue
              const t = Date.parse(u.expiresAt)
              u.expiresAt = t < Date.now() ? endOfDay(Date.now() + days * DAY) : iso(t + days * DAY)
            }
            changed++
          }
          if (action === 'delete') s.users = s.users.filter((u) => !names.includes(u.name))
          return send(200, { changed })
        }
        const um = rest.match(/^users\/([^/]+)(?:\/(.+))?$/)
        if (um) {
          const u = s.users.find((x) => x.name === decodeURIComponent(um[1]))
          if (!u) return fail(404, 'no such user')
          const action = um[2]
          if (!action && method === 'PATCH') {
            if (body.note !== undefined && String(body.note).trim().length > 80) return fail(400, 'the note must be at most 80 characters')
            apply(u, body)
            return send(200, userView(s, u))
          }
          if (!action && method === 'DELETE') {
            s.users.splice(s.users.indexOf(u), 1)
            return send(200, { ok: true })
          }
          if (action === 'rotate-secret' && method === 'POST') {
            u.secret = hex(32)
            return send(200, userView(s, u))
          }
          if (action === 'reset-usage' && method === 'POST') {
            u.up = 0
            u.down = 0
            return send(200, userView(s, u))
          }
        }
        return fail(404, 'not found')
      })
    },
  }
}

function apply(u: MockUser, b: Record<string, unknown>) {
  if (typeof b.enabled === 'boolean') u.enabled = b.enabled
  if (b.maxIps !== undefined) u.maxIps = Number(b.maxIps) || 0
  if (b.quotaBytes !== undefined) u.quotaBytes = Number(b.quotaBytes) || 0
  if (b.expiresAt !== undefined) u.expiresAt = (b.expiresAt as string | null) || null
  if (b.note !== undefined) u.note = String(b.note).trim()
}

const LOG_OK = [
  'agent: mtproto-agent {agent} starting',
  'agent: link tcp-1 up (rtt 38 ms)',
  'agent: link tcp-2 up (rtt 44 ms)',
  'agent: applied state {rev} ({users} users)',
  'telemt: Telemt MTProxy v{telemt}',
  'telemt: listening on 0.0.0.0:443',
  'telemt: handshake ok user=alice dc=2 from [ip]',
  'telemt: handshake ok user=family dc=4 from [ip]',
  'telemt: bad handshake from [ip], passed to the cover site',
  'agent: dc dial 192.0.2.80:443 failed: i/o timeout',
  'telemt: handshake ok user=office dc=2 from [ip]',
  'telemt: warning: client [ip] sent an unknown TLS extension',
]
const LOG_CRASH = [
  'agent: starting telemt (try {n})',
  'telemt: Telemt MTProxy v{telemt}',
  'telemt: cannot bind 0.0.0.0:443: address already in use',
  'agent: telemt exited (exit status 1), restart {n}',
  'agent: telemt keeps crashing; waiting 60 s',
]

// One line every 5 s, the same lines for the same moments, so the page sees new lines arrive.
function logs(s: MockServer, lines: number) {
  const src = s.telemt.crashLoop ? LOG_CRASH : LOG_OK
  const slot = 5000
  const last = Math.floor(Date.now() / slot)
  const count = Math.max(1, Math.min(lines, 1000))
  return Array.from({ length: count }, (_, k) => {
    const n = last - count + 1 + k
    const line = src[n % src.length]
      .replace('{agent}', s.agentVersion)
      .replace('{telemt}', s.telemtVersion)
      .replace('{rev}', 'r' + hexOf(s.coverDomain).slice(0, 6))
      .replace('{users}', String(s.users.length))
      .replace(/\{n\}/g, String((Math.floor(n / src.length) % 90) + 10))
    return `${iso(n * slot)} ${line}`
  })
}

seed()
