// A fake plugin API for `npm run dev:mock`: the whole UI runs with sample data
// and no router, SSH server or telemt. Development only; never built into the page.
//
// Sample servers cover every state (running, stopped, crashed, unreachable,
// router key revoked), and connection and traffic numbers move every few seconds.
//
// Add-server scenarios, picked by the address you type:
//   contains "fail"     -> install fails at "Check the server" (no sudo rights)
//   contains "offline"  -> the host key check cannot connect
//   contains "changed"  -> the host key differs from last time
//   contains "kept"     -> an existing telemt config (and its users) is kept
//   an existing address -> the host key is already known, so no confirmation
import type { Plugin } from 'vite'

const DAY = 86400e3
const now = () => Math.floor(Date.now() / 1000)
const hex = (n: number) =>
  Array.from({ length: n }, () => Math.floor(Math.random() * 16).toString(16)).join('')
const hexOf = (s: string) => Buffer.from(s).toString('hex')
const rand = (lo: number, hi: number) => lo + Math.floor(Math.random() * (hi - lo + 1))
const ip = () => `${rand(5, 223)}.${rand(0, 255)}.${rand(0, 255)}.${rand(1, 254)}`
const inDays = (d: number) => new Date(Date.now() + d * DAY).toISOString().replace(/\.\d+Z$/, 'Z')

interface User {
  username: string
  secret: string
  enabled: boolean
  max_tcp_conns: number | null
  expiration_rfc3339: string | null
  data_quota_bytes: number | null
  rate_limit_up_bps: number | null
  rate_limit_down_bps: number | null
  max_unique_ips: number | null
  current_connections: number
  ips: string[]
  total_octets: number
  /** How busy the user is; drives the live numbers. 0 = never online. */
  activity: number
}

interface Server {
  id: string
  name: string
  host: string
  sshPort: number
  username: string
  fingerprint: string
  proxyPort: number
  tlsDomain: string
  arch: string
  addedAt: number
}

interface State {
  service: 'active' | 'inactive' | 'failed' | 'activating'
  version: string
  startedAt: number
  /** Status calls fail with this, like an SSH error from the real router. */
  unreachable?: string
}

function user(username: string, o: Partial<User> = {}): User {
  const u: User = {
    username,
    secret: hex(32),
    enabled: true,
    max_tcp_conns: null,
    expiration_rfc3339: null,
    data_quota_bytes: null,
    rate_limit_up_bps: null,
    rate_limit_down_bps: null,
    max_unique_ips: null,
    current_connections: 0,
    ips: [],
    total_octets: 0,
    activity: 0,
    ...o,
  }
  if (u.activity > 0 && u.enabled && u.current_connections === 0) {
    u.current_connections = rand(1, 2 + u.activity * 2)
    u.ips = Array.from({ length: Math.min(u.current_connections, rand(1, 3)) }, ip)
  }
  return u
}

function wire(server: Server, u: User) {
  const link = `tg://proxy?server=${server.host}&port=${server.proxyPort}&secret=ee${u.secret}${hexOf(server.tlsDomain)}`
  return {
    username: u.username,
    enabled: u.enabled,
    in_runtime: true,
    user_ad_tag: null,
    max_tcp_conns: u.max_tcp_conns,
    expiration_rfc3339: u.expiration_rfc3339,
    data_quota_bytes: u.data_quota_bytes,
    rate_limit_up_bps: u.rate_limit_up_bps,
    rate_limit_down_bps: u.rate_limit_down_bps,
    max_unique_ips: u.max_unique_ips,
    current_connections: u.current_connections,
    active_unique_ips: u.ips.length,
    active_unique_ips_list: u.ips,
    recent_unique_ips: u.ips.length + (u.activity ? 1 : 0),
    recent_unique_ips_list: u.ips,
    total_octets: u.total_octets,
    links: {
      classic: [],
      secure: [],
      // A dual-stack server gets one link per address, like telemt reports.
      tls: server.id === 'fra1' ? [link, link.replace(server.host, '2001:db8::10')] : [link],
    },
  }
}

const GB = 1e9
let servers: Server[] = []
let state: Record<string, State> = {}
let users: Record<string, User[]> = {}

function seed() {
servers = [
  { id: 'fra1', name: 'Frankfurt VPS', host: '203.0.113.10', sshPort: 22, username: 'root', fingerprint: 'SHA256:p2OiwWbYoEAsmmYWEMy1L12ya7lUDzuHYODmtJ18R/w', proxyPort: 443, tlsDomain: 'www.google.com', arch: 'x86_64', addedAt: now() - 86400 * 41 },
  { id: 'hel1', name: 'Helsinki ARM', host: 'hel.example.net', sshPort: 2222, username: 'deploy', fingerprint: 'SHA256:FkImfecZh+m0r8bQdJoD77DxMU9mViXNaqX/kA9ZISU', proxyPort: 8443, tlsDomain: 'www.cloudflare.com', arch: 'aarch64', addedAt: now() - 86400 * 9 },
  { id: 'ams1', name: 'Amsterdam backup', host: '198.51.100.77', sshPort: 22, username: 'root', fingerprint: 'SHA256:3c4Q9zqk0d1s0l2mUu5l0cT8m2o9aXq7r6c4e1YwZ0A', proxyPort: 443, tlsDomain: 'www.microsoft.com', arch: 'x86_64', addedAt: now() - 86400 * 120 },
  { id: 'ist1', name: 'Istanbul test', host: 'ist.example.org', sshPort: 22, username: 'ubuntu', fingerprint: 'SHA256:Wk2m8l1QeY0Jr7pZr0cN4hQ1vX7eQ2tM3uK9sL5dA1B', proxyPort: 443, tlsDomain: 'www.apple.com', arch: 'x86_64', addedAt: now() - 86400 * 3 },
  { id: 'old1', name: 'Old Hetzner', host: '192.0.2.44', sshPort: 22, username: 'root', fingerprint: 'SHA256:Qy1Zr5o0cV9lKp3mW2xE7tU4aB6dN8sF0gH1jK2lM3n', proxyPort: 443, tlsDomain: 'www.google.com', arch: 'x86_64', addedAt: now() - 86400 * 400 },
  { id: 'nyc1', name: 'New York', host: 'nyc.example.com', sshPort: 22, username: 'admin', fingerprint: 'SHA256:Ab3Cd5Ef7Gh9Ij1Kl3Mn5Op7Qr9St1Uv3Wx5Yz7Ab9C', proxyPort: 993, tlsDomain: 'outlook.office365.com', arch: 'x86_64', addedAt: now() - 86400 * 60 },
]

state = {
  fra1: { service: 'active', version: '3.5.9', startedAt: now() - 86400 * 6 - 3600 * 5 },
  hel1: { service: 'active', version: '3.5.9', startedAt: now() - 3600 * 20 },
  ams1: { service: 'inactive', version: '3.5.7', startedAt: now() },
  ist1: { service: 'failed', version: '3.5.9', startedAt: now() },
  old1: { service: 'active', version: '3.4.2', startedAt: now(), unreachable: 'timed out connecting to 192.0.2.44:22' },
  nyc1: { service: 'active', version: '3.5.9', startedAt: now(), unreachable: 'the server no longer accepts the router key for admin; it was removed from authorized_keys, or key login is disabled' },
}

}

const FIRST = ['alice', 'bob', 'carol', 'dave', 'erin', 'frank', 'grace', 'heidi', 'ivan', 'judy', 'mallory', 'nina', 'oscar', 'peggy', 'rupert', 'sybil', 'trent', 'victor', 'walter', 'zoe']

function seedUsers() {
users = {
  fra1: [
    user('alice', { activity: 2, total_octets: 3.81 * GB, data_quota_bytes: 50 * GB, max_unique_ips: 3, expiration_rfc3339: inDays(24) }),
    user('bob', { total_octets: 0.91 * GB, max_tcp_conns: 20 }),
    user('family', { activity: 3, total_octets: 148.3 * GB, rate_limit_down_bps: 20e6, rate_limit_up_bps: 5e6, max_unique_ips: 8 }),
    user('old-phone', { enabled: false, total_octets: 0.012 * GB, expiration_rfc3339: inDays(-3) }),
    user('office', { activity: 3, total_octets: 612.4 * GB, max_tcp_conns: 200, max_unique_ips: 40 }),
    user('trial-7d', { activity: 1, total_octets: 0.24 * GB, data_quota_bytes: 5 * GB, expiration_rfc3339: inDays(6) }),
    user('expired-trial', { total_octets: 4.99 * GB, data_quota_bytes: 5 * GB, expiration_rfc3339: inDays(-12) }),
    user('mom.tablet', { activity: 1, total_octets: 7.2 * GB }),
    user('dad_laptop', { total_octets: 21.6 * GB, expiration_rfc3339: inDays(0.2) }),
    user('guest-wifi', { enabled: false, total_octets: 0 }),
    user('streamer', { activity: 3, total_octets: 1812 * GB, rate_limit_down_bps: 50e6, data_quota_bytes: 2000 * GB }),
    user('a-very-long-user-name.with.dots_and-dashes-2026', { activity: 1, total_octets: 0.05 * GB, expiration_rfc3339: inDays(365) }),
    ...FIRST.slice(2, 12).map((n, i) =>
      user(`${n}-${2020 + i}`, {
        activity: i % 3 === 0 ? 1 : 0,
        total_octets: rand(1, 90_000) * 1e6,
        data_quota_bytes: i % 4 === 0 ? 100 * GB : null,
        max_unique_ips: i % 2 ? 2 : null,
        expiration_rfc3339: i % 5 === 0 ? inDays(rand(10, 200)) : null,
        enabled: i !== 7,
      }),
    ),
  ],
  hel1: [
    user('user1', { activity: 1, total_octets: 2.1 * GB }),
    user('team-a', { activity: 2, total_octets: 55 * GB, max_unique_ips: 10, max_tcp_conns: 64 }),
    user('team-b', { total_octets: 18 * GB, max_unique_ips: 10 }),
    user('backup-link', { enabled: false }),
    user('conference', { total_octets: 0.3 * GB, expiration_rfc3339: inDays(1.5) }),
  ],
  ams1: [user('spare1', { total_octets: 12 * GB }), user('spare2')],
  ist1: [user('tester')],
  old1: [],
  nyc1: [],
}
}
seed()
seedUsers()

// Live numbers: busy users come and go and keep using traffic. Started by the
// dev server only, so importing this file (as vite.config.ts does) never keeps
// a `vite build` process alive.
function tick() {
  for (const [id, list] of Object.entries(users)) {
    if (state[id].service !== 'active') continue
    for (const u of list) {
      if (!u.enabled || !u.activity) continue
      const cap = u.max_tcp_conns ?? 4 + u.activity * 4
      u.current_connections = Math.max(0, Math.min(cap, u.current_connections + rand(-2, 2)))
      if (u.current_connections === 0) u.ips = []
      else if (u.ips.length === 0 || (Math.random() < 0.2 && u.ips.length < (u.max_unique_ips ?? 4)))
        u.ips = [...u.ips, ip()]
      else if (u.ips.length > u.current_connections) u.ips = u.ips.slice(0, u.current_connections)
      u.total_octets += u.current_connections * rand(20_000, 400_000) * u.activity
    }
  }
}

const LOG_OK = [
  'INFO telemt::maestro::bootstrap: Telemt MTProxy v3.5.9',
  'INFO telemt::maestro::bootstrap: Modes: classic=false secure=false tls=true',
  'INFO telemt::api: control API listening on 127.0.0.1:9091',
  'INFO telemt::proxy: listening on 0.0.0.0:443',
  'INFO telemt::proxy::client: handshake ok user=alice dc=2',
  'INFO telemt::proxy::client: handshake ok user=family dc=4',
  'WARN telemt::proxy::client: bad handshake from 45.83.12.9 (masked to www.google.com:443)',
  'INFO telemt::config: reloaded configuration (users: 22)',
  'INFO telemt::proxy::client: handshake ok user=office dc=2',
  'WARN telemt::access: user trial-7d rejected: unique IP limit reached',
]
const LOG_FAILED = [
  'INFO telemt::maestro::bootstrap: Telemt MTProxy v3.5.9',
  'ERROR telemt::proxy: cannot bind 0.0.0.0:443: Address already in use (os error 98)',
  'systemd[1]: telemt.service: Main process exited, code=exited, status=1/FAILURE',
  'systemd[1]: telemt.service: Failed with result \'exit-code\'.',
]

function logs(id: string, lines: number) {
  const src = state[id].service === 'failed' ? LOG_FAILED : state[id].service === 'inactive' ? [...LOG_OK.slice(0, 4), 'systemd[1]: Stopped telemt.service - telemt MTProto proxy.'] : LOG_OK
  const start = Date.now() - src.length * 60e3 * 3
  const out = []
  for (let i = 0; i < Math.min(lines, 120); i++) {
    const line = src[i % src.length]
    const ts = new Date(start + i * 47e3).toISOString().replace(/\.\d+Z$/, '+0000')
    out.push(`${ts} ${line.startsWith('systemd') ? 'vps ' + line : `vps telemt[812]: ${line}`}`)
  }
  return out.join('\n')
}

type Job = {
  id: number
  running: boolean
  steps: { title: string; state: string; detail: string }[]
  error: string | null
  report: unknown
}
let job: Job = { id: 0, running: false, steps: [], error: null, report: null }

function runJob(body: Record<string, string | number>) {
  const host = String(body.host)
  const fail = host.includes('fail')
  const kept = host.includes('kept')
  const sudo = body.username !== 'root'
  const steps: [string, string][] = [
    [`Connect to ${host}:${body.sshPort}`, `Logged in as ${body.username}`],
    ['Check the server', `x86_64, systemd, ${sudo ? 'sudo' : 'root'}`],
    ['Download the proxy', 'Version 3.5.9, 16.7 MB'],
    ['Upload the proxy and its files', ''],
    ['Install and start the proxy', kept ? 'Kept the existing configuration and its users' : 'New configuration'],
    ['Give the router its own key', 'Key login works; the password is not kept'],
    ['Wait for the proxy to answer', kept ? '3 user(s)' : '1 user(s)'],
    ['Save the server', String(body.name || host)],
  ]
  const run = fail ? steps.slice(0, 2) : steps
  job = { id: job.id + 1, running: true, steps: [], error: null, report: null }
  const step = 650
  run.forEach(([title, detail], i) => {
    setTimeout(() => job.steps.push({ title, state: 'running', detail: '' }), i * step)
    setTimeout(() => {
      if (fail && i === 1) {
        const error = `Using sudo (the user needs sudo rights) failed (exit 1): ${body.username} is not in the sudoers file.`
        Object.assign(job.steps[i], { state: 'failed', detail: error })
        job.error = error
        job.running = false
        return
      }
      Object.assign(job.steps[i], { state: 'done', detail })
    }, i * step + (i === 2 ? 1500 : 500))
  })
  if (fail) return
  setTimeout(() => {
    const id = hex(8)
    servers.push({ id, name: String(body.name || host), host, sshPort: Number(body.sshPort), username: String(body.username), fingerprint: 'SHA256:u3Rq0Q1uV3Jc0mYw7b6o8xk3j2m9Pq5c1f0aZyXwVb8', proxyPort: Number(body.proxyPort), tlsDomain: String(body.tlsDomain), arch: 'x86_64', addedAt: now() })
    users[id] = kept ? [user('kept-1', { total_octets: 3 * GB }), user('kept-2'), user(String(body.firstUser))] : [user(String(body.firstUser))]
    state[id] = { service: 'active', version: '3.5.9', startedAt: now() }
    job.running = false
    job.report = { serverId: id, release: '3.5.9', keptConfig: kept, users: users[id].length }
  }, run.length * step + 1200)
}

export function mockApi(): Plugin {
  return {
    name: 'mock-api',
    configureServer(server) {
      const ticker = setInterval(tick, 2500)
      server.httpServer?.on('close', () => clearInterval(ticker))
      server.middlewares.use('/api', async (req, res) => {
        const chunks: Buffer[] = []
        for await (const c of req) chunks.push(c as Buffer)
        const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString() || '{}') : {}
        const method = req.method ?? 'GET'
        const [rawPath, query = ''] = (req.url ?? '').split('?')
        const path = rawPath.replace(/^\//, '')
        const send = (status: number, data: unknown) => {
          res.statusCode = status
          res.setHeader('Content-Type', 'application/json')
          res.end(JSON.stringify(data))
        }
        const fail = (status: number, message: string) => send(status, { error: message })
        const ok = (data: unknown, status = 200) => send(status, { ok: true, data, revision: 'r' })
        const telemtError = (status: number, code: string, message: string) =>
          send(status, { ok: false, error: { code, message } })
        await new Promise((r) => setTimeout(r, rand(120, 350)))

        // Test hook: back to the sample data.
        if (path === '__reset' && method === 'POST') {
          seed()
          seedUsers()
          job = { id: 0, running: false, steps: [], error: null, report: null }
          return send(200, { ok: true })
        }
        if (path === 'config')
          return send(200, {
            version: '0.1.0',
            repo: 'telemt/telemt',
            releaseTag: '',
            routerKey: 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHic9wJyadk0MockKeyOnlyForTheDemo0000000 nasnet-mtproto-remote',
          })
        if (path === 'probe') {
          const host = String(body.host)
          if (host.includes('offline')) return fail(502, `could not connect to ${host}:${body.sshPort}: Connection refused (os error 111)`)
          const known = servers.find((s) => s.host === host)
          return send(200, {
            fingerprint: known?.fingerprint ?? 'SHA256:u3Rq0Q1uV3Jc0mYw7b6o8xk3j2m9Pq5c1f0aZyXwVb8',
            status: host.includes('changed') ? 'changed' : known ? 'known' : 'new',
          })
        }
        if (path === 'deploy') {
          if (job.running) return fail(409, 'An installation is already running.')
          runJob(body)
          return send(202, { id: job.id })
        }
        if (path === 'job') return send(200, job)
        if (path === 'servers') return send(200, servers)

        const m = path.match(/^servers\/([^/]+)(?:\/(.*))?$/)
        if (!m) return fail(404, 'not found')
        const [, id, rest = ''] = m
        const srv = servers.find((s) => s.id === id)
        if (!srv) return fail(404, 'no such server')
        const st = state[id]
        const list = users[id]

        if (rest === '' && method === 'PATCH') {
          srv.name = body.name
          return send(200, srv)
        }
        if (rest === '' && method === 'DELETE') {
          if (st.unreachable && body.uninstall) return fail(502, st.unreachable)
          servers.splice(servers.indexOf(srv), 1)
          return send(200, {
            removed: true,
            warning: st.unreachable ? `Removed from the list, but the server could not be cleaned up: ${st.unreachable}` : null,
          })
        }
        // Everything below needs an SSH connection to the server.
        if (st.unreachable) return fail(502, st.unreachable)

        if (rest === 'status') {
          const up = st.service === 'active'
          return send(200, {
            service: st.service,
            info: up ? { version: st.version, uptime_seconds: now() - st.startedAt } : null,
            summary: up ? { configured_users: list.length, connections_total: 81_244 } : null,
            apiError: up ? null : 'request to the server\'s port 9091 failed: Connection refused (os error 111)',
          })
        }
        if (rest.startsWith('service/')) {
          const action = rest.split('/')[1]
          if (action === 'stop') st.service = 'inactive'
          else {
            st.service = 'active'
            st.startedAt = now()
          }
          return send(200, { ok: true })
        }
        if (rest === 'logs') return send(200, { logs: logs(id, Number(new URLSearchParams(query).get('lines') ?? 200)) })
        if (rest === 'update') {
          await new Promise((r) => setTimeout(r, 2500))
          st.version = '3.6.0'
          st.service = 'active'
          st.startedAt = now()
          return send(200, { release: '3.6.0' })
        }

        if (st.service !== 'active') return fail(502, 'request to the server\'s port 9091 failed: Connection refused (os error 111)')
        const t = rest.replace(/^telemt\/v1\//, '')
        const u = t.match(/^users\/([^/]+)(?:\/(.+))?$/)
        if (t === 'users' && method === 'GET') return ok(list.map((x) => wire(srv, x)))
        if (t === 'users' && method === 'POST') {
          if (!/^[A-Za-z0-9_.-]{1,64}$/.test(body.username ?? ''))
            return telemtError(400, 'bad_request', 'username must match [A-Za-z0-9_.-] and be 1..64 chars')
          if (list.some((x) => x.username === body.username)) return telemtError(409, 'user_exists', 'User already exists')
          const { username, ...limits } = body
          const created = user(username, limits)
          list.push(created)
          return ok({ user: wire(srv, created), secret: created.secret }, 202)
        }
        if (u) {
          const target = list.find((x) => x.username === decodeURIComponent(u[1]))
          if (!target) return telemtError(404, 'not_found', 'User not found')
          if (method === 'GET') return ok(wire(srv, target))
          if (method === 'PATCH') {
            Object.assign(target, body)
            return ok(wire(srv, target))
          }
          if (method === 'DELETE') {
            list.splice(list.indexOf(target), 1)
            return ok({ username: target.username, in_runtime: true }, 202)
          }
          if (u[2] === 'enable' || u[2] === 'disable') {
            target.enabled = u[2] === 'enable'
            if (!target.enabled) {
              target.current_connections = 0
              target.ips = []
            }
            return ok(wire(srv, target))
          }
          if (u[2] === 'rotate-secret') {
            target.secret = hex(32)
            return ok({ user: wire(srv, target), secret: target.secret })
          }
          if (u[2] === 'reset-quota') return ok({ username: target.username, used_bytes: 0, last_reset_epoch_secs: now() })
        }
        if (t === 'config' && method === 'PATCH') {
          srv.tlsDomain = body.censorship.tls_domain
          return ok({ changed: ['censorship'], restart_required: true })
        }
        return fail(403, 'that proxy API route is not allowed')
      })
    },
  }
}
