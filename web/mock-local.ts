// A fake Local MTProto API for `npm run dev:local:mock`: the Local page runs with
// sample data and no router. Development only; never built into the page.
//
// The proxy is set up: cover site www.example.com, five users, some online, the
// router at port 41873 with IP Cloud name 7a1b2c3d4e5f.sn.mynetname.net. "New
// port…" moves the router to another port after 3 s. A cover site containing
// "bad" fails its check.
//
// The e2e reset takes { fresh } (not set up yet: the setup card), { noCloud } (no
// IP Cloud name, so links have no address), { noDomestic } (a single-link router),
// { unreported } (the router script hasn't written yet), { crash } (the proxy keeps
// crashing) and { stopped } (stopped by the owner).
// Every other call needs the page's X-MTProto-UI header, as the plugin does.
import type { Plugin } from 'vite'
import type { CoverCheck, LocalProxy, ProxyOptions, ServerEvent, User } from './src/lib/api'

const DAY = 86400e3
const MIN = 60e3
const GB = 1e9
const MB = 1e6
const STEP = 15e3
const VERSION = '1.0.0'
const TELEMT = '3.4.25'
const NAME = /^[A-Za-z0-9_.-]{1,32}$/
const DOMAIN = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$/
const iso = (ms: number) => new Date(ms).toISOString().replace(/\.\d+Z$/, 'Z')
const ago = (sec: number) => iso(Date.now() - sec * 1000)
const hex = (n: number) => Array.from({ length: n }, () => Math.floor(Math.random() * 16).toString(16)).join('')
const hexOf = (s: string) => Buffer.from(s).toString('hex')
const rand = (lo: number, hi: number) => lo + Math.floor(Math.random() * (hi - lo + 1))
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))
const endOfDay = (ms: number) => `${new Date(ms).toISOString().slice(0, 10)}T23:59:59Z`

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
  /** online while above 0 and the proxy runs */
  activity: number
  devs: number
  lastSeen: number | null
  today: number
  month: number
}

interface State {
  createdAt: string
  cover: string
  users: MockUser[]
  stopped: boolean
  options: ProxyOptions
  maxConns: number
  linkHost: string
  linkPort: number
  router: { reported: boolean; domestic: boolean; port: number; cloud: string; public: string; wan: string; status: string; at: number }
  asked: number | null
  telemt: { running: boolean; restarts: number; start: number; crashLoop: boolean; lastExit: string }
  coverCheckedAt: number | null
  coverOk: boolean
  events: ServerEvent[]
  samples: { at: string; up: number; down: number }[]
  rate: { up: number; down: number }
  started: number
}

function user(name: string, o: Partial<MockUser> = {}): MockUser {
  return {
    name, secret: hex(32), enabled: true, maxIps: 0, expiresAt: null, quotaBytes: 0, up: 0, down: 0,
    createdAt: ago(86400 * 12), note: '', activity: 0, devs: 1, lastSeen: null, today: 0, month: 0, ...o,
  }
}

const split = (total: number) => {
  const up = Math.round(total * 0.18)
  return { up, down: Math.round(total) - up }
}
const used = (total: number, month = total, today = 0) => ({ ...split(total), month, today })

let s: State

function seed(o: Record<string, unknown> = {}) {
  const now = Date.now()
  s = {
    createdAt: ago(86400 * 12),
    cover: 'www.example.com',
    users: [
      user('mom', { note: "Mom's phone", activity: 2, devs: 1, ...used(6.2 * GB, 4.1 * GB, 180 * MB), lastSeen: now }),
      user('dad', { note: 'Dad, tablet and phone', activity: 1, devs: 2, ...used(3.4 * GB, 2.2 * GB, 90 * MB), maxIps: 3, lastSeen: now }),
      user('sara', { note: 'Sara at uni', ...used(11.8 * GB, 7.5 * GB), quotaBytes: 20 * GB, expiresAt: iso(now + 24 * DAY), lastSeen: now - 40 * MIN }),
      user('ali', { note: 'Cousin', ...used(0.4 * GB), lastSeen: now - 3 * DAY }),
      user('guest', { note: 'Week pass', enabled: false, ...used(1.1 * GB), expiresAt: endOfDay(now - DAY), lastSeen: now - 2 * DAY }),
    ],
    stopped: false,
    options: { extraCoverNames: [], deviceWindowSecs: 0, logLevel: 'normal' },
    maxConns: 0,
    linkHost: '',
    linkPort: 0,
    router: { reported: true, domestic: true, port: 41873, cloud: '7a1b2c3d4e5f.sn.mynetname.net', public: '203.0.113.57', wan: '192.168.1.20', status: 'updated', at: now - 3 * 3600e3 },
    asked: null,
    telemt: { running: true, restarts: 0, start: now - 2 * DAY - 3 * 3600e3, crashLoop: false, lastExit: '' },
    coverCheckedAt: now - 2 * 3600e3,
    coverOk: true,
    events: [
      { at: ago(2 * 86400 + 3 * 3600), kind: 'telemt_up', detail: '' },
      { at: ago(12 * 86400), kind: 'port_changed', detail: 'from 23110 to 41873' },
    ],
    samples: Array.from({ length: 60 }, (_, i) => {
      const at = now - (59 - i) * STEP
      const d = Math.round(380e3 * (1 + 0.35 * Math.sin(i / 4) + (Math.random() - 0.5) * 0.3))
      return { at: iso(at), up: Math.round(d * 0.22), down: d }
    }),
    rate: { up: 80e3, down: 380e3 },
    started: now - 2 * DAY - 3 * 3600e3,
  }
  if (o.fresh) Object.assign(s, { cover: '', users: [], coverCheckedAt: null, events: [], telemt: { ...s.telemt, running: false, start: now } })
  if (o.noCloud) Object.assign(s.router, { cloud: '', public: '', status: 'unsupported' })
  if (o.noDomestic) Object.assign(s.router, { domestic: false, port: 0, wan: '' })
  if (o.unreported) Object.assign(s.router, { reported: false, port: 0, cloud: '', public: '', wan: '', status: '' })
  if (o.crash) Object.assign(s.telemt, { running: false, crashLoop: true, restarts: 23, lastExit: 'exit status 1' })
  if (o.stopped) s.stopped = true
}

const held = () =>
  s.router.reported && !s.router.domestic
    ? 'this router has no domestic link'
    : !s.cover
      ? 'waiting for a cover site'
      : s.stopped
        ? 'stopped from the panel'
        : !s.users.length
          ? 'it starts when there is a user'
          : ''
const runs = () => s.telemt.running && !held() && Date.now() >= s.telemt.start
const state = (u: MockUser): User['state'] =>
  !u.enabled ? 'disabled' : u.expiresAt && Date.parse(u.expiresAt) < Date.now() ? 'expired' : u.quotaBytes && u.up + u.down >= u.quotaBytes ? 'over-quota' : 'active'
const online = (u: MockUser) => runs() && u.activity > 0 && state(u) === 'active'
const defaultConns = 1000

function address() {
  const host = s.linkHost || s.router.cloud || s.router.public
  return { host, port: s.linkPort || s.router.port }
}

function links(u: MockUser, domain: string) {
  const a = address()
  const q = `server=${a.host}&port=${a.port}&secret=ee${u.secret}${hexOf(domain)}`
  return { tg: `tg://proxy?${q}`, https: `https://t.me/proxy?${q}` }
}

function userView(u: MockUser): User {
  const on = online(u)
  const t = split(u.today)
  const m = split(u.month)
  return {
    name: u.name, secret: u.secret, enabled: u.enabled, maxIps: u.maxIps, expiresAt: u.expiresAt, quotaBytes: u.quotaBytes,
    up: u.up, down: u.down, createdAt: u.createdAt, note: u.note, state: state(u),
    online: on, connections: on ? u.activity * 2 + 1 : 0, devices: on ? u.devs : 0,
    lastSeenAt: on ? ago(3) : u.lastSeen ? iso(u.lastSeen) : null,
    todayUp: t.up, todayDown: t.down, monthUp: m.up, monthDown: m.down,
    links: { ...links(u, s.cover), extra: s.options.extraCoverNames.map((domain) => ({ domain, ...links(u, domain) })) },
  }
}

function view(): LocalProxy {
  const now = Date.now()
  const on = s.users.filter(online)
  const sum = (k: 'today' | 'month') => split(s.users.reduce((n, u) => n + u[k], 0))
  const a = address()
  const r = runs()
  return {
    version: VERSION,
    arch: 'arm64',
    createdAt: s.createdAt,
    coverDomain: s.cover,
    userCount: s.users.length,
    proxyStopped: s.stopped,
    proxyOptions: { ...s.options, extraCoverNames: [...s.options.extraCoverNames] },
    maxConns: { value: s.maxConns || defaultConns, default: defaultConns, min: 100, max: 5000 },
    proxy: {
      running: r, restarts: s.telemt.restarts, uptimeSec: r ? Math.floor((now - s.telemt.start) / 1000) : 0,
      lastExit: s.telemt.lastExit, crashLoop: s.telemt.crashLoop, held: held(), applyError: '',
      version: TELEMT, build: '', rssBytes: r ? 18_300_000 + on.length * 1_100_000 : 0, port: 8443, reportedAt: iso(now),
    },
    cover: { domain: s.cover, ok: s.coverOk, checkedAt: s.coverCheckedAt ? iso(s.coverCheckedAt) : null, error: s.coverOk ? '' : 'cannot connect to ' + s.cover + ':443 from the router' },
    address: {
      host: a.host, port: a.port, customHost: s.linkHost, customPort: s.linkPort, routerPort: s.router.port,
      cloudName: s.router.cloud, publicAddress: s.router.public, wanAddress: s.router.wan, cloudStatus: s.router.status,
      domestic: s.router.domestic, reported: s.router.reported, reportedAt: s.router.reported ? iso(s.router.at) : null,
      newPortAskedAt: s.asked ? iso(s.asked) : null,
    },
    usage: {
      live: true, usersOnline: on.length, connections: on.reduce((n, u) => n + u.activity * 2 + 1, 0), devices: on.reduce((n, u) => n + u.devs, 0),
      rateUp: r ? Math.round(s.rate.up) : 0, rateDown: r ? Math.round(s.rate.down) : 0,
      todayUp: sum('today').up, todayDown: sum('today').down, monthUp: sum('month').up, monthDown: sum('month').down, updatedAt: ago(4),
    },
    events: s.events.slice(0, 50),
    uptimeSec: Math.floor((now - s.started) / 1000),
    rssBytes: 14_700_000,
  }
}

const event = (kind: string, detail = '') => s.events.unshift({ at: iso(Date.now()), kind, detail })

function tick() {
  const now = Date.now()
  if (s.asked && now - s.asked > 3000) {
    const from = s.router.port
    s.router.port = rand(20000, 60000)
    s.router.at = now
    s.asked = null
    event('port_changed', `from ${from} to ${s.router.port}`)
  }
  if (!runs()) return
  let down = 0
  for (const u of s.users.filter(online)) {
    const d = u.activity * rand(20e3, 120e3)
    const sp = split(d)
    u.up += sp.up
    u.down += sp.down
    u.today += d
    u.month += d
    u.lastSeen = now
    down += sp.down
  }
  s.rate = { up: down * 0.22 / 2.5, down: down / 2.5 }
  const last = s.samples[s.samples.length - 1]
  if (!last || now - Date.parse(last.at) >= STEP) {
    s.samples.push({ at: iso(now), up: Math.round(s.rate.up), down: Math.round(s.rate.down) })
    s.samples = s.samples.slice(-60)
  }
}

function check(domain: string): CoverCheck {
  const ok = !domain.includes('bad')
  return {
    ok, tls13: ok, x25519: ok, h2: ok, certOk: ok, addrs: ok ? ['93.184.215.14'] : [], latencyMs: ok ? rand(30, 90) : 0,
    error: ok ? '' : `cannot connect to ${domain}:443 from the router`,
  }
}

function apply(u: MockUser, b: Record<string, unknown>) {
  if (typeof b.enabled === 'boolean') u.enabled = b.enabled
  if (b.maxIps !== undefined) u.maxIps = Number(b.maxIps) || 0
  if (b.quotaBytes !== undefined) u.quotaBytes = Number(b.quotaBytes) || 0
  if (b.expiresAt !== undefined) u.expiresAt = (b.expiresAt as string | null) || null
  if (b.note !== undefined) u.note = String(b.note).trim()
}

const LOG = [
  'telemt: Telemt MTProxy v{telemt}',
  'telemt: listening on 0.0.0.0:8443',
  'telemt: handshake ok user=mom dc=2 from [ip]',
  'telemt: handshake ok user=dad dc=4 from [ip]',
  'telemt: bad handshake from [ip], passed to the cover site',
  'plugin: the router moved the proxy to port {port}',
]
const LOG_CRASH = ['telemt: Telemt MTProxy v{telemt}', 'telemt: cannot bind 0.0.0.0:8443: address already in use', 'plugin: telemt exited (exit status 1)']

function logs(lines: number) {
  const src = s.telemt.crashLoop ? LOG_CRASH : LOG
  const slot = 5000
  const last = Math.floor(Date.now() / slot)
  const count = Math.max(1, Math.min(lines, 1000))
  return Array.from({ length: count }, (_, k) => {
    const n = last - count + 1 + k
    return `${iso(n * slot)} ${src[n % src.length].replace('{telemt}', TELEMT).replace('{port}', String(s.router.port))}`
  })
}

export function mockLocal(): Plugin {
  return {
    name: 'mock-local',
    configureServer(server) {
      const ticker = setInterval(tick, 2500)
      server.httpServer?.on('close', () => clearInterval(ticker))
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
        const fail = (status: number, error: string) => send(status, { error })
        await sleep(rand(60, 180))
        if (path !== '__reset' && req.headers['x-mtproto-ui'] !== '1') return fail(403, 'requests from other sites are refused')
        if (path === '__reset' && method === 'POST') {
          seed(body)
          return send(200, { ok: true })
        }
        if (path === 'config' && method === 'GET') return send(200, { version: VERSION, flavor: 'local' })
        if (path === 'local' && method === 'GET') return send(200, view())
        if (path === 'local/setup' && method === 'POST') {
          if (s.cover) return fail(409, 'the proxy is set up already')
          const cover = String(body.coverDomain ?? '').trim().toLowerCase()
          const name = String(body.firstUser ?? '').trim()
          if (!DOMAIN.test(cover)) return fail(400, 'the cover site must be a domain name, like www.example.com')
          if (!NAME.test(name)) return fail(400, "a user name is 1 to 32 letters, digits, '.', '_' or '-'")
          await sleep(600)
          const c = check(cover)
          if (!c.ok) return fail(400, `${cover} does not work as a cover site from this router: ${c.error}`)
          Object.assign(s, { cover, coverCheckedAt: Date.now(), coverOk: true })
          s.users.push(user(name, { createdAt: iso(Date.now()) }))
          s.telemt = { ...s.telemt, running: true, start: Date.now() + 1500 }
          event('telemt_up', '')
          return send(200, view())
        }
        if (path === 'local/settings' && method === 'PATCH') {
          if (body.coverDomain !== undefined) {
            const cover = String(body.coverDomain).trim().toLowerCase()
            if (!DOMAIN.test(cover)) return fail(400, 'the cover site must be a domain name, like www.example.com')
            const c = check(cover)
            if (!c.ok) return fail(400, `${cover} does not work as a cover site from this router: ${c.error}`)
            Object.assign(s, { cover, coverCheckedAt: Date.now(), coverOk: true })
          }
          if (body.maxConns !== undefined) {
            const n = Number(body.maxConns)
            if (n !== 0 && (n < 100 || n > 5000)) return fail(400, 'the connection limit is 100 to 5000')
            s.maxConns = n
            s.telemt.start = Date.now() + 1500
          }
          if (body.linkHost !== undefined) {
            const h = String(body.linkHost).trim().toLowerCase()
            if (h && !DOMAIN.test(h) && !/^\d{1,3}(\.\d{1,3}){3}$/.test(h)) return fail(400, 'the address must be a domain name or an IPv4 address, like home.example.com')
            s.linkHost = h
          }
          if (body.linkPort !== undefined) {
            const p = Number(body.linkPort)
            if (!Number.isInteger(p) || p < 0 || p > 65535) return fail(400, "the port is 1 to 65535, or empty for the router's own")
            s.linkPort = p
          }
          if (body.proxyOptions) {
            const o = body.proxyOptions
            if (Array.isArray(o.extraCoverNames)) {
              if (o.extraCoverNames.some((n: string) => n.includes('bad'))) return fail(400, 'a cover name failed its check')
              s.options.extraCoverNames = o.extraCoverNames
            }
            if (o.deviceWindowSecs !== undefined) s.options.deviceWindowSecs = Number(o.deviceWindowSecs)
            if (o.logLevel !== undefined) s.options.logLevel = o.logLevel
          }
          return send(200, view())
        }
        if (path === 'local/new-port' && method === 'POST') {
          if (!s.router.reported || !s.router.domestic) return fail(409, "the router hasn't set the proxy's port up yet")
          s.asked = Date.now()
          return send(200, view())
        }
        if (path === 'local/restart' && method === 'POST') {
          if (s.stopped) return fail(409, 'start the proxy first')
          s.telemt = { ...s.telemt, running: true, crashLoop: false, start: Date.now() + 2500 }
          event('telemt_up', '')
          return send(200, { ok: true })
        }
        if (path === 'local/proxy' && method === 'POST') {
          s.stopped = !!body.stopped
          if (!s.stopped) s.telemt.start = Date.now() + 1500
          return send(200, view())
        }
        if (path === 'local/disguise-check' && method === 'POST') {
          await sleep(500)
          return send(200, check(String(body.domain ?? '')))
        }
        if (path === 'local/logs' && method === 'GET') return send(200, { lines: logs(Number(new URLSearchParams(query).get('lines') ?? 300)) })
        if (path === 'local/traffic' && method === 'GET') return send(200, { stepSec: 15, samples: s.samples })

        if (!s.cover && path.startsWith('local/users')) return fail(409, 'set the proxy up first')
        if (path === 'local/users' && method === 'GET') return send(200, s.users.map(userView))
        if (path === 'local/users' && method === 'POST') {
          const name = String(body.name ?? '')
          if (!NAME.test(name) || /^\.+$/.test(name)) return fail(400, "a user name is 1 to 32 letters, digits, '.', '_' or '-'")
          if (s.users.some((u) => u.name === name)) return fail(409, `there is already a user called ${name}`)
          const u = user(name, { createdAt: iso(Date.now()), note: String(body.note ?? '').trim() })
          if (body.secret) u.secret = String(body.secret).slice(-32)
          apply(u, body)
          s.users.push(u)
          return send(201, userView(u))
        }
        if (path === 'local/users/bulk' && method === 'POST') {
          const names: string[] = Array.isArray(body.names) ? body.names.map(String) : []
          if (!names.length) return fail(400, 'pick the users to change')
          let changed = 0
          for (const u of s.users.filter((x) => names.includes(x.name))) {
            if (body.action === 'pause') u.enabled = false
            else if (body.action === 'resume') u.enabled = true
            else if (body.action === 'renew') {
              if (!u.expiresAt) continue
              const t = Date.parse(u.expiresAt)
              u.expiresAt = t < Date.now() ? endOfDay(Date.now() + 30 * DAY) : iso(t + 30 * DAY)
            }
            changed++
          }
          if (body.action === 'delete') s.users = s.users.filter((u) => !names.includes(u.name))
          return send(200, { changed })
        }
        const um = path.match(/^local\/users\/([^/]+)(?:\/(.+))?$/)
        if (um) {
          const u = s.users.find((x) => x.name === decodeURIComponent(um[1]))
          if (!u) return fail(404, 'no such user')
          if (!um[2] && method === 'PATCH') {
            apply(u, body)
            return send(200, userView(u))
          }
          if (!um[2] && method === 'DELETE') {
            s.users.splice(s.users.indexOf(u), 1)
            return send(200, { ok: true })
          }
          if (um[2] === 'rotate-secret' && method === 'POST') {
            u.secret = hex(32)
            return send(200, userView(u))
          }
          if (um[2] === 'reset-usage' && method === 'POST') {
            u.up = 0
            u.down = 0
            return send(200, userView(u))
          }
        }
        return fail(404, 'no such route')
      })
    },
  }
}

seed()
