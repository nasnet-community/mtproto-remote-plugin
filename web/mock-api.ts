// A fake plugin API for `npm run dev:mock`: the whole UI runs with sample data
// and no router, VPS or agent. Development only; never built into the page.
//
// Sample servers cover each link state: Frankfurt is up, Helsinki is degraded
// with a crash-looping proxy, Amsterdam's link is down (removing it needs a
// login or force). Traffic and link numbers move every few seconds. The router
// is at its cap of 3 servers; remove one to try adding.
//
// Add-server scenarios, picked by the address or cover site you type:
//   address contains "fail"    -> fails at "Check the server" (no systemd)
//   address contains "nolink"  -> fails at the link step (not in the domestic list)
//   address contains "offline" -> the host key check cannot connect
//   address contains "changed" -> the host key differs from last time
//   address contains "poll"    -> the job stream is unavailable, so the UI polls
//   cover site contains "bad"  -> the cover-site check fails
//   an existing address        -> refused: it is already a server here (repair it instead)
//
// Helsinki's cover site is not checked yet (its agent just restarted). The e2e
// reset takes { maxServers, allowlistUrl, slowRemove } to set up other cases, and
// { rebuilt } (Amsterdam presents a new host key) or { halfInstalled } (Amsterdam's
// install died before it was saved, so it can only be removed).
// Every other call needs the page's X-MTProto-UI header, as the plugin does.
import { readFileSync } from 'node:fs'
import type { IncomingMessage, ServerResponse } from 'node:http'
import type { Plugin } from 'vite'
import type { AgentState, Allowlist, CoverCheck, Job, JobStep, LinkConn, LinkState, Server, ServerEvent, User } from './src/lib/api'

const DAY = 86400e3
const iso = (ms: number) => new Date(ms).toISOString().replace(/\.\d+Z$/, 'Z')
const ago = (sec: number) => iso(Date.now() - sec * 1000)
const inDays = (d: number) => iso(Date.now() + d * DAY)
const hex = (n: number) => Array.from({ length: n }, () => Math.floor(Math.random() * 16).toString(16)).join('')
const hexOf = (s: string) => Buffer.from(s).toString('hex')
const rand = (lo: number, hi: number) => lo + Math.floor(Math.random() * (hi - lo + 1))
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

// What an update installs: the versions the plugin pins.
const AGENT = '0.5.0'
const TELEMT = '3.4.25'
const GB = 1e9
const BUDGET = 6000
const DEFAULT_STREAMS = 2000
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
  /** How busy the user is; drives the moving numbers. */
  activity: number
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
  arch: string
  agentVersion: string
  telemtVersion: string
  /** How many of the two link connections are up. */
  connsUp: number
  /** When the connections that are down dropped. */
  downAt: number
  lastSeen: number
  agentStart: number
  /** When the cover site was last checked; null: not yet. */
  coverCheckedAt: number | null
  telemt: { running: boolean; restarts: number; start: number; lastExit: string; crashLoop: boolean }
  stats: { streams: number; relayed: number; dialFailures: number; violations: number; toDc: number; fromDc: number; active: number; rejected: number }
  users: MockUser[]
  events: ServerEvent[]
}

function user(name: string, o: Partial<MockUser> = {}): MockUser {
  return { name, secret: hex(32), enabled: true, maxIps: 0, expiresAt: null, quotaBytes: 0, up: 0, down: 0, createdAt: ago(86400 * rand(1, 40)), activity: 0, ...o }
}

// Splits a total roughly the way proxy traffic goes: mostly down.
const traffic = (total: number) => ({ up: Math.round(total * 0.18), down: Math.round(total * 0.82) })

function userState(u: MockUser): User['state'] {
  if (!u.enabled) return 'disabled'
  if (u.expiresAt && Date.parse(u.expiresAt) < Date.now()) return 'expired'
  if (u.quotaBytes && u.up + u.down >= u.quotaBytes) return 'over-quota'
  return 'active'
}

function userView(s: MockServer, u: MockUser): User {
  const { activity: _, ...rest } = u
  const q = `server=${s.host}&port=443&secret=ee${u.secret}${hexOf(s.coverDomain)}`
  return { ...rest, state: userState(u), links: { tg: `tg://proxy?${q}`, https: `https://t.me/proxy?${q}` } }
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
  const at = s.connsUp ? Date.now() - 4000 : s.lastSeen
  return {
    version: s.agentVersion,
    telemtVersion: s.telemtVersion,
    uptimeSec: Math.floor((at - s.agentStart) / 1000),
    rssBytes: 21_400_000 + s.users.length * 40_000,
    stateRev: 'r' + hexOf(s.coverDomain).slice(0, 6),
    reportedAt: iso(at),
    telemt: {
      running: s.telemt.running,
      restarts: s.telemt.restarts,
      uptimeSec: s.telemt.running ? Math.floor((at - s.telemt.start) / 1000) : 0,
      lastExit: s.telemt.lastExit,
      crashLoop: s.telemt.crashLoop,
    },
    capture: { rules: true, active: s.connsUp ? s.stats.active : 0, rejected: s.stats.rejected },
    cover: s.coverCheckedAt
      ? { domain: s.coverDomain, ok: true, checkedAt: iso(s.coverCheckedAt), error: '' }
      : { domain: s.coverDomain, ok: false, checkedAt: null, error: '' },
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
  }
  if (detail) out.events = s.events.slice(0, 50)
  return out
}

const event = (s: MockServer, kind: string, detail = '', at = Date.now()) =>
  s.events.unshift({ at: iso(at), kind, detail })

const FIRST = ['carol', 'dave', 'erin', 'frank', 'grace', 'heidi', 'ivan', 'judy', 'mallory', 'nina']

let servers: MockServer[] = []
let nextId = 1
let maxServers = 3
let allowlist: Allowlist
/** Removal jobs wait this long after forgetting the server, like a slow last event. */
let removeTail = 0

function seed() {
  const h = 3600e3
  const fra: MockServer = {
    id: 1, name: 'Frankfurt VPS', host: '203.0.113.10', sshPort: 22, sshUser: 'root', hostKey: 'SHA256:p2OiwWbYoEAsmmYWEMy1L12ya7lUDzuHYODmtJ18R/w', registered: true,
    createdAt: ago(86400 * 41), coverDomain: 'www.example.com', maxStreams: 2000, hubPort: 8443, arch: 'amd64', agentVersion: AGENT, telemtVersion: TELEMT,
    connsUp: 2, downAt: 0, lastSeen: Date.now(), agentStart: Date.now() - 6 * DAY - 5 * h, coverCheckedAt: Date.now() - h,
    telemt: { running: true, restarts: 0, start: Date.now() - 6 * DAY - 5 * h, lastExit: '', crashLoop: false },
    stats: { streams: 412, relayed: 1_884_201, dialFailures: 3, violations: 0, toDc: 412 * GB, fromDc: 1906 * GB, active: 388, rejected: 2 },
    users: [
      user('alice', { activity: 2, ...traffic(3.81 * GB), quotaBytes: 50 * GB, maxIps: 3, expiresAt: inDays(24) }),
      user('bob', { ...traffic(0.91 * GB) }),
      user('family', { activity: 3, ...traffic(148.3 * GB), maxIps: 8 }),
      user('old-phone', { enabled: false, ...traffic(0.012 * GB), expiresAt: inDays(-3) }),
      user('office', { activity: 3, ...traffic(612.4 * GB), maxIps: 40 }),
      user('trial-7d', { activity: 1, ...traffic(0.24 * GB), quotaBytes: 5 * GB, expiresAt: inDays(6) }),
      user('expired-trial', { ...traffic(3.2 * GB), quotaBytes: 5 * GB, expiresAt: inDays(-12) }),
      user('used-up', { ...traffic(10 * GB), quotaBytes: 10 * GB }),
      user('mom.tablet', { activity: 1, ...traffic(7.2 * GB) }),
      user('dad_laptop', { ...traffic(21.6 * GB), expiresAt: inDays(0.2) }),
      user('guest-wifi', { enabled: false }),
      user('a-long-user-name.with.dots_2026', { activity: 1, ...traffic(0.05 * GB), expiresAt: inDays(365) }),
      ...FIRST.map((n, i) =>
        user(`${n}-${2020 + i}`, {
          activity: i % 3 === 0 ? 1 : 0,
          ...traffic(rand(1, 90_000) * 1e6),
          quotaBytes: i % 4 === 0 ? 100 * GB : 0,
          maxIps: i % 2 ? 2 : 0,
          expiresAt: i % 5 === 0 ? inDays(rand(10, 200)) : null,
          enabled: i !== 7,
        }),
      ),
    ],
    events: [],
  }
  event(fra, 'link_up', '2 of 2 connections', fra.agentStart)
  event(fra, 'telemt_up', `telemt ${TELEMT}`, fra.agentStart + 2000)
  event(fra, 'dc_dial_failed', 'dial 192.0.2.80:443: i/o timeout', Date.now() - 2 * h)

  const hel: MockServer = {
    id: 2, name: 'Helsinki ARM', host: 'hel.example.net', sshPort: 2222, sshUser: 'deploy', hostKey: 'SHA256:FkImfecZh+m0r8bQdJoD77DxMU9mViXNaqX/kA9ZISU', registered: true,
    createdAt: ago(86400 * 9), coverDomain: 'static.example.net', maxStreams: 1000, hubPort: 8443, arch: 'arm64', agentVersion: '0.4.2', telemtVersion: '3.4.20',
    connsUp: 1, downAt: Date.now() - 2 * h, lastSeen: Date.now(), agentStart: Date.now() - 20 * h, coverCheckedAt: null,
    telemt: { running: false, restarts: 14, start: Date.now() - 20 * h, lastExit: 'exit status 1', crashLoop: true },
    stats: { streams: 0, relayed: 20_310, dialFailures: 0, violations: 3, toDc: 4.1 * GB, fromDc: 22 * GB, active: 0, rejected: 7 },
    users: [
      user('user1', { activity: 1, ...traffic(2.1 * GB) }),
      user('team-a', { activity: 2, ...traffic(55 * GB), maxIps: 10 }),
      user('team-b', { ...traffic(18 * GB), maxIps: 10 }),
      user('backup-link', { enabled: false }),
      user('conference', { ...traffic(0.3 * GB), expiresAt: inDays(1.5) }),
    ],
    events: [],
  }
  event(hel, 'link_up', '2 of 2 connections', hel.agentStart)
  event(hel, 'allowlist_violation', '3 dials refused in 5 minutes, last to 198.51.100.9:443', Date.now() - 3 * h)
  event(hel, 'link_down', 'tcp-2: i/o timeout', Date.now() - 2 * h)
  event(hel, 'telemt_down', 'exit status 1', Date.now() - 40 * 60e3)
  event(hel, 'telemt_crash_loop', '14 restarts; backing off', Date.now() - 25 * 60e3)

  const ams: MockServer = {
    id: 3, name: 'Amsterdam backup', host: '198.51.100.77', sshPort: 22, sshUser: 'root', hostKey: 'SHA256:3c4Q9zqk0d1s0l2mUu5l0cT8m2o9aXq7r6c4e1YwZ0A', registered: true,
    createdAt: ago(86400 * 120), coverDomain: 'www.example.org', maxStreams: 1000, hubPort: 8443, arch: 'amd64', agentVersion: AGENT, telemtVersion: TELEMT,
    connsUp: 0, downAt: Date.now() - 3 * h, lastSeen: Date.now() - 3 * h, agentStart: Date.now() - 2 * DAY, coverCheckedAt: Date.now() - 4 * h,
    telemt: { running: true, restarts: 0, start: Date.now() - 2 * DAY, lastExit: '', crashLoop: false },
    stats: { streams: 0, relayed: 99_120, dialFailures: 0, violations: 0, toDc: 9 * GB, fromDc: 40 * GB, active: 0, rejected: 0 },
    users: [user('spare1', { ...traffic(12 * GB) }), user('spare2')],
    events: [],
  }
  event(ams, 'link_up', '2 of 2 connections', ams.agentStart)
  event(ams, 'link_down', 'both connections: i/o timeout', ams.lastSeen)

  servers = [fra, hel, ams]
  nextId = 4
  maxServers = 3
  removeTail = 0
  allowlist = { prefixes: 1, placeholder: true, refreshedAt: null, lastError: '', url: 'https://lists.example.net/allowlist.txt' }
  jobs.clear()
  installing.clear()
}

// Moving numbers. Started by the dev server only, so importing this file (as
// vite.config.ts does) never keeps a `vite build` process alive.
function tick() {
  for (const s of servers) {
    if (!s.connsUp || !s.telemt.running) continue
    s.stats.streams = Math.max(0, s.stats.streams + rand(-20, 20))
    s.stats.active = Math.max(0, s.stats.streams - rand(0, 30))
    s.stats.relayed += rand(5, 60)
    for (const u of s.users) {
      if (userState(u) !== 'active' || !u.activity) continue
      const bytes = rand(20_000, 400_000) * u.activity * 4
      u.up += Math.round(bytes * 0.18)
      u.down += Math.round(bytes * 0.82)
      s.stats.toDc += Math.round(bytes * 0.18)
      s.stats.fromDc += Math.round(bytes * 0.82)
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
}

const snapshot = (j: MockJob): Job => ({ ...j.view, steps: j.view.steps.map((s) => ({ ...s })) })
const emit = (j: MockJob) => j.listeners.forEach((l) => l(snapshot(j)))

function startJob(kind: Job['kind'], steps: Step[], o: { serverId?: number; noStream?: boolean; tail?: number; done?: () => void; end?: () => void } = {}) {
  const j: MockJob = {
    view: { id: hex(16), kind, state: 'running', steps: [], serverId: o.serverId, startedAt: iso(Date.now()) },
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
      const s: JobStep = { name: step.name, state: 'running' }
      j.view.steps.push(s)
      emit(j)
      await sleep(step.ms ?? 380)
      if (step.fail) {
        Object.assign(s, { state: 'failed', detail: step.fail })
        return finish(step.fail)
      }
      Object.assign(s, { state: 'ok', detail: step.detail })
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

// SSH-only steps of a reinstall; repair runs them over a kept configuration.
const reinstall = (s: MockServer): Step[] => [
  { name: 'check', detail: `${s.arch} Ubuntu 24.04` },
  { name: 'verify' },
  { name: 'account', detail: 'uid 998' },
  { name: 'upload', ms: 600 },
  { name: 'directories' },
  { name: 'disguise', detail: s.coverDomain },
  { name: 'credentials', detail: 'kept' },
  { name: 'service' },
]

function bringUp(s: MockServer) {
  s.connsUp = 2
  s.lastSeen = Date.now()
  s.agentStart = Date.now()
  s.telemt = { running: true, restarts: 0, start: Date.now(), lastExit: '', crashLoop: false }
  event(s, 'link_up', '2 of 2 connections')
}

function install(b: Record<string, unknown>) {
  const host = String(b.host)
  const cover = String(b.coverDomain)
  const name = String(b.name || host)
  const first = String(b.firstUser ?? '')
  let s: MockServer | undefined
  installing.add(host)
  return startJob(
    'install',
    [
      // tested before the job started (see POST servers)
      { name: 'disguise', detail: cover, ms: 0 },
      { name: 'download', detail: `agent ${AGENT}, telemt ${TELEMT}`, ms: 700 },
      { name: 'check', detail: 'amd64 Ubuntu 24.04', fail: host.includes('fail') && 'this server does not run systemd' },
      { name: 'verify' },
      { name: 'account', detail: 'uid 998' },
      { name: 'upload', ms: 600 },
      { name: 'directories' },
      { name: 'credentials' },
      { name: 'service' },
      {
        name: 'register',
        detail: name,
        then: (j) => {
          s = {
            id: nextId++, name, host, sshPort: Number(b.sshPort) || 22, sshUser: String(b.sshUser || 'root'), hostKey: String(b.hostKey), registered: true,
            createdAt: iso(Date.now()), coverDomain: cover, maxStreams: Number(b.maxStreams) || DEFAULT_STREAMS, hubPort: 8443, arch: 'amd64',
            agentVersion: AGENT, telemtVersion: TELEMT, connsUp: 0, downAt: Date.now(), lastSeen: Date.now(), agentStart: Date.now(), coverCheckedAt: Date.now(),
            telemt: { running: true, restarts: 0, start: Date.now(), lastExit: '', crashLoop: false },
            stats: { streams: 0, relayed: 0, dialFailures: 0, violations: 0, toDc: 0, fromDc: 0, active: 0, rejected: 0 },
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
        fail: host.includes('nolink') && 'the agent is running but the router cannot reach it on TCP port 8443',
        then: () => s && bringUp(s),
      },
    ],
    { noStream: host.includes('poll'), end: () => installing.delete(host) },
  )
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
        if (path !== '__reset' && !stream && req.headers['x-mtproto-ui'] !== '1')
          return fail(403, 'requests from other sites are refused')

        // Test hook: back to the sample data, optionally with a different cap.
        if (path === '__reset' && method === 'POST') {
          seed()
          if (body.maxServers) maxServers = Number(body.maxServers)
          if (typeof body.allowlistUrl === 'string') allowlist.url = body.allowlistUrl
          if (body.slowRemove) removeTail = 6000
          const ams = servers[2]
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
          if (!String(body.host ?? '').trim()) return fail(400, 'give the server address')
          if (!DOMAIN.test(String(body.coverDomain ?? ''))) return fail(400, 'give the cover site as a domain name')
          const login = badLogin(body)
          if (login) return fail(400, login)
          if (body.maxStreams !== undefined && (!Number.isInteger(body.maxStreams) || body.maxStreams < 100))
            return fail(400, 'max streams must be at least 100')
          const left = BUDGET - allocated()
          if ((Number(body.maxStreams) || DEFAULT_STREAMS) > left) return fail(409, `only ${left} streams are left on this router`)
          // like the plugin: the login and the cover site are tested before the job starts
          await sleep(900)
          if (body.password === 'wrong') return fail(502, 'ssh: unable to authenticate: the password or key was not accepted')
          const cover = String(body.coverDomain).trim().toLowerCase()
          const check = coverCheck(cover)
          if (!check.ok)
            return fail(422, `${cover} does not work as a cover site from this VPS: ${check.error}`, { cover: { ...check, latencyMs: 0 } })
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
        if (rest === 'settings' && method === 'PATCH') {
          if (body.maxStreams !== undefined) {
            const n = Number(body.maxStreams)
            if (!Number.isInteger(n) || n < 100 || n > 20000) return fail(400, 'max streams must be 100 to 20000')
            const left = BUDGET - allocated(s)
            if (n > left) return fail(409, `only ${left} streams are left on this router`)
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
          return send(200, view(s, true))
        }
        if (rest === 'disguise-check' && method === 'POST') {
          if (linkDown()) return fail(503, linkDown())
          await sleep(600)
          return send(200, coverCheck(String(body.domain ?? '').trim().toLowerCase()))
        }
        if (rest === 'repair' && method === 'POST') {
          const login = badLogin(body)
          if (login) return fail(400, login)
          const steps = reinstall(s)
          if (body.password === 'wrong') steps[0].fail = 'ssh: unable to authenticate: the password or key was not accepted'
          else steps[0].fail = wrongKey(s, body)
          // a repair that worked pins the key it logged in with
          steps[0].then = () => ((s.hostKey = String(body.hostKey)), delete s.presentedKey)
          if (!s.connsUp) steps.push({ name: 'link', ms: 600, then: () => bringUp(s) })
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
          if (body.kind === 'agent')
            return started(
              startJob('update-agent', [
                { name: 'download', detail: `agent ${AGENT}` },
                { name: 'upload', ms: 600 },
                { name: 'restart', ms: 800, then: () => ((s.agentVersion = AGENT), (s.agentStart = Date.now())) },
                { name: 'commit' },
              ], { serverId: s.id }),
            )
          return started(
            startJob('update-telemt', [
              { name: 'download', detail: `telemt ${TELEMT}` },
              { name: 'upload', ms: 800, then: () => ((s.telemtVersion = TELEMT), (s.telemt.start = Date.now())) },
            ], { serverId: s.id }),
          )
        }
        if (rest === 'rotate' && method === 'POST') {
          if (linkDown()) return fail(503, linkDown())
          return started(startJob('rotate', [{ name: 'rotate' }, { name: 'verify', ms: 700 }], { serverId: s.id }))
        }
        if (rest === 'restart' && method === 'POST') {
          if (linkDown()) return fail(503, linkDown())
          await sleep(500)
          s.telemt = { ...s.telemt, running: true, crashLoop: false, start: Date.now() }
          event(s, 'telemt_up', 'restarted from the router')
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
          const u = user(name, { createdAt: iso(Date.now()) })
          apply(u, body)
          s.users.push(u)
          return send(201, userView(s, u))
        }
        const um = rest.match(/^users\/([^/]+)(?:\/(.+))?$/)
        if (um) {
          const u = s.users.find((x) => x.name === decodeURIComponent(um[1]))
          if (!u) return fail(404, 'no such user')
          const action = um[2]
          if (!action && method === 'PATCH') {
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
}

const LOG_OK = [
  'agent: mtproto-agent {agent} starting',
  'agent: link tcp-1 up (rtt 38 ms)',
  'agent: link tcp-2 up (rtt 44 ms)',
  'agent: applied state {rev} ({users} users)',
  'telemt: Telemt MTProxy v{telemt}',
  'telemt: listening on 0.0.0.0:443',
  'telemt: handshake ok user=alice dc=2 from 5.x.x.x',
  'telemt: handshake ok user=family dc=4 from 31.x.x.x',
  'telemt: bad handshake from 45.x.x.x, passed to the cover site',
  'agent: relay: dc dial 192.0.2.80:443 failed: i/o timeout',
  'telemt: handshake ok user=office dc=2 from 89.x.x.x',
]
const LOG_CRASH = [
  'agent: mtproto-agent {agent} starting',
  'agent: link tcp-1 up (rtt 51 ms)',
  'telemt: Telemt MTProxy v{telemt}',
  'telemt: cannot bind 0.0.0.0:443: address already in use',
  'agent: telemt exited (exit status 1), restart 14',
  'agent: telemt keeps crashing; backing off 60s',
]

function logs(s: MockServer, lines: number) {
  const src = s.telemt.crashLoop ? LOG_CRASH : LOG_OK
  const start = Date.now() - src.length * 47e3
  return Array.from({ length: Math.min(lines, src.length * 6) }, (_, i) => {
    const line = src[i % src.length]
      .replace('{agent}', s.agentVersion)
      .replace('{telemt}', s.telemtVersion)
      .replace('{rev}', 'r' + hexOf(s.coverDomain).slice(0, 6))
      .replace('{users}', String(s.users.length))
    return `${new Date(start + i * 47e3).toISOString().replace(/\.\d+Z$/, 'Z')} ${line}`
  })
}

seed()
