// A fake plugin API for `npm run dev:mock`: the whole UI runs with sample
// data and no router, SSH server or telemt. Development only; not built.
import type { Plugin } from 'vite'

type User = Record<string, unknown> & { username: string; enabled: boolean }

const now = () => Math.floor(Date.now() / 1000)
const hex = (n: number) =>
  Array.from({ length: n }, () => Math.floor(Math.random() * 16).toString(16)).join('')

function makeUser(name: string, extra: Partial<User> = {}): User {
  const secret = hex(32)
  return {
    username: name,
    enabled: true,
    in_runtime: true,
    max_tcp_conns: null,
    expiration_rfc3339: null,
    data_quota_bytes: null,
    rate_limit_up_bps: null,
    rate_limit_down_bps: null,
    max_unique_ips: null,
    current_connections: 0,
    active_unique_ips: 0,
    active_unique_ips_list: [],
    recent_unique_ips: 0,
    total_octets: 0,
    links: {
      classic: [],
      secure: [],
      tls: [`tg://proxy?server=203.0.113.10&port=443&secret=ee${secret}7777772e676f6f676c652e636f6d`],
    },
    ...extra,
  }
}

const servers = [
  { id: 'a1b2c3d4', name: 'Frankfurt VPS', host: '203.0.113.10', sshPort: 22, username: 'root', fingerprint: 'SHA256:p2OiwWbYoEAsmmYWEMy1L12ya7lUDzuHYODmtJ18R/w', proxyPort: 443, tlsDomain: 'www.google.com', arch: 'x86_64', addedAt: now() - 86400 * 12 },
  { id: 'e5f6a7b8', name: 'Helsinki', host: 'hel.example.net', sshPort: 2222, username: 'deploy', fingerprint: 'SHA256:FkImfecZh+m0r8bQdJoD77DxMU9mViXNaqX/kA9ZISU', proxyPort: 8443, tlsDomain: 'www.cloudflare.com', arch: 'aarch64', addedAt: now() - 86400 * 2 },
]

const users: Record<string, User[]> = {
  a1b2c3d4: [
    makeUser('alice', { current_connections: 3, active_unique_ips: 2, active_unique_ips_list: ['198.51.100.4', '198.51.100.9'], total_octets: 3_812_000_000, data_quota_bytes: 50_000_000_000, max_unique_ips: 3, expiration_rfc3339: new Date(Date.now() + 86400e3 * 24).toISOString() }),
    makeUser('bob', { total_octets: 912_000_000, max_tcp_conns: 20 }),
    makeUser('family', { current_connections: 7, active_unique_ips: 4, total_octets: 48_300_000_000, rate_limit_down_bps: 20_000_000 }),
    makeUser('old-phone', { enabled: false, total_octets: 12_000_000, expiration_rfc3339: new Date(Date.now() - 86400e3 * 3).toISOString() }),
  ],
  e5f6a7b8: [makeUser('user1')],
}
const service: Record<string, string> = { a1b2c3d4: 'active', e5f6a7b8: 'active' }

let job = { id: 0, running: false, steps: [] as { title: string; state: string; detail: string }[], error: null as string | null, report: null as unknown }

function runJob(host: string, firstUser: string) {
  const steps = [
    [`Connect to ${host}:22`, 'Logged in as root'],
    ['Check the server', 'x86_64, systemd, root'],
    ['Download telemt from telemt/telemt', 'telemt-x86_64-linux-musl.tar.gz (3.5.9, 16.7 MB)'],
    ['Upload telemt and its files', ''],
    ['Install and start telemt', 'New configuration'],
    ['Give the router its own key', 'Key login works; the password is not kept'],
    ['Wait for telemt to answer', '1 user(s)'],
    ['Save the server', host],
  ]
  job = { id: job.id + 1, running: true, steps: [], error: null, report: null }
  steps.forEach(([title, detail], i) => {
    setTimeout(() => job.steps.push({ title, state: 'running', detail: '' }), i * 700)
    setTimeout(() => Object.assign(job.steps[i], { state: 'done', detail }), i * 700 + 600)
  })
  setTimeout(() => {
    const id = hex(8)
    servers.push({ id, name: host, host, sshPort: 22, username: 'root', fingerprint: 'SHA256:new', proxyPort: 443, tlsDomain: 'www.google.com', arch: 'x86_64', addedAt: now() })
    users[id] = [makeUser(firstUser)]
    service[id] = 'active'
    job.running = false
    job.report = { serverId: id, release: '3.5.9', keptConfig: false, users: 1 }
  }, steps.length * 700 + 100)
}

export function mockApi(): Plugin {
  return {
    name: 'mock-api',
    configureServer(server) {
      server.middlewares.use('/api', async (req, res) => {
        const chunks: Buffer[] = []
        for await (const c of req) chunks.push(c as Buffer)
        const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString() || '{}') : {}
        const method = req.method ?? 'GET'
        const path = (req.url ?? '').split('?')[0].replace(/^\//, '')
        const send = (status: number, data: unknown) => {
          res.statusCode = status
          res.setHeader('Content-Type', 'application/json')
          res.end(JSON.stringify(data))
        }
        const ok = (data: unknown, status = 200) => send(status, { ok: true, data, revision: 'r' })
        await new Promise((r) => setTimeout(r, 150))

        if (path === 'config') return send(200, { version: '0.1.0', repo: 'telemt/telemt', releaseTag: '', routerKey: 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMockKeyForTheDemoOnly0000000000000000000 nasnet-mtproto-remote' })
        if (path === 'probe') return send(200, { fingerprint: 'SHA256:u3Rq0Q1uV3Jc0mYw7b6o8xk3j2m9Pq5c1f0aZyXwVb8', status: 'new' })
        if (path === 'deploy') { runJob(body.host, body.firstUser); return send(202, { id: job.id }) }
        if (path === 'job') return send(200, job)
        if (path === 'servers') return send(200, servers)

        const m = path.match(/^servers\/([^/]+)(?:\/(.*))?$/)
        if (!m) return send(404, { error: 'not found' })
        const [, id, rest = ''] = m
        const srv = servers.find((s) => s.id === id)
        if (!srv) return send(404, { error: 'no such server' })
        const list = users[id]

        if (rest === '' && method === 'PATCH') { srv.name = body.name; return send(200, srv) }
        if (rest === '' && method === 'DELETE') { servers.splice(servers.indexOf(srv), 1); return send(200, { removed: true, warning: null }) }
        if (rest === 'status') {
          return send(200, { service: service[id], info: service[id] === 'active' ? { version: '3.5.9', uptime_seconds: 3600 * 49 } : null, summary: { configured_users: list.length, connections_total: 812 }, apiError: service[id] === 'active' ? null : 'telemt is not running' })
        }
        if (rest.startsWith('service/')) { service[id] = rest.endsWith('stop') ? 'inactive' : 'active'; return send(200, { ok: true }) }
        if (rest === 'logs') return send(200, { logs: Array.from({ length: 30 }, (_, i) => `2026-09-29T18:${String(i).padStart(2, '0')}:00+0000 vps telemt[812]: INFO accepted connection user=alice dc=2`).join('\n') })
        if (rest === 'update') return send(200, { release: '3.6.0' })

        const t = rest.replace(/^telemt\/v1\//, '')
        const u = t.match(/^users\/([^/]+)(?:\/(.+))?$/)
        if (t === 'users' && method === 'GET') return ok(list)
        if (t === 'users' && method === 'POST') {
          if (list.some((x) => x.username === body.username)) return send(409, { ok: false, error: { code: 'user_exists', message: 'User already exists' } })
          const created = makeUser(body.username, body)
          list.push(created)
          return ok({ user: created, secret: hex(32) }, 201)
        }
        if (u) {
          const user = list.find((x) => x.username === decodeURIComponent(u[1]))
          if (!user) return send(404, { ok: false, error: { code: 'not_found', message: 'User not found' } })
          if (method === 'PATCH') { Object.assign(user, body); return ok(user) }
          if (method === 'DELETE') { list.splice(list.indexOf(user), 1); return ok({ username: user.username }, 202) }
          if (u[2] === 'enable' || u[2] === 'disable') { user.enabled = u[2] === 'enable'; return ok(user) }
          if (u[2] === 'rotate-secret') { Object.assign(user, { links: makeUser(user.username).links }); return ok({ user, secret: hex(32) }) }
          if (u[2] === 'reset-quota') return ok({ username: user.username, used_bytes: 0 })
        }
        if (t === 'config' && method === 'PATCH') { srv.tlsDomain = body.censorship.tls_domain; return ok({ changed: ['censorship'] }) }
        return send(403, { error: 'that telemt API route is not allowed' })
      })
    },
  }
}
