// Typed calls to the plugin's API. URLs are relative, so they resolve against
// the <base href> the server injects (the panel's plugin path).

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly body: Record<string, unknown> = {},
  ) {
    super(message)
  }

  /** The server's link is down and the call needs an SSH login (or force). */
  get needLogin() {
    return this.body.needLogin === true
  }
}

// Another site's page can't send this without a preflight, which the panel refuses.
const OWN = { 'X-MTProto-UI': '1' }

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`api/${path}`, {
    method,
    headers: body === undefined ? OWN : { ...OWN, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) {
    const message = typeof data?.error === 'string' && data.error ? data.error : undefined
    throw new ApiError(message ?? `Request failed (${res.status})`, res.status, data ?? {})
  }
  return data as T
}

export interface Allowlist {
  prefixes: number
  placeholder: boolean
  refreshedAt: string | null
  lastError: string
  url: string
}

export interface AppConfig {
  version: string
  maxServers: number
  serverCount: number
  streamBudget: number
  streamsAllocated: number
  defaultMaxStreams: number
  allowlist: Allowlist
}

export interface ProbeRequest {
  host: string
  sshPort: number
  sshUser: string
}

export interface ProbeResult {
  fingerprint: string
  status: 'new' | 'known' | 'changed'
}

export interface Login {
  sshPort: number
  sshUser: string
  password?: string
  privateKey?: string
  passphrase?: string
  hostKey: string
}

export interface InstallRequest extends Login {
  name: string
  host: string
  coverDomain: string
  maxStreams?: number
  firstUser?: string
  allowUnverifiedCover?: boolean
  hubPort?: number
  capturePort?: number
}

export interface RepairRequest extends Login {
  reset?: boolean
}

export interface RemoveRequest {
  force?: boolean
  login?: Login
}

export type JobState = 'running' | 'ok' | 'failed'

export interface JobStep {
  name: string
  state: JobState | 'pending'
  detail?: string
}

export type JobKind = 'install' | 'repair' | 'uninstall' | 'update-agent' | 'update-telemt' | 'rotate' | 'link-port'

export interface Job {
  id: string
  kind: JobKind
  state: JobState
  steps: JobStep[]
  error?: string
  /** 0 or missing when the job has no server (yet) */
  serverId?: number
  /** the install's server name, else "" */
  target?: string
  startedAt: string
  finishedAt?: string
}

export interface LinkConn {
  name: string
  up: boolean
  since: string | null
  rttMs: number
  streams: number
  lastError: string
}

export interface LinkState {
  state: 'up' | 'degraded' | 'down'
  up: number
  total: number
  streams: number
  relayed: number
  dialFailures: number
  violations: number
  bytesToDc: number
  bytesFromDc: number
  conns: LinkConn[]
}

export interface AgentState {
  version: string
  telemtVersion: string
  uptimeSec: number
  rssBytes: number
  stateRev: string
  reportedAt: string
  /** held: why the agent keeps the proxy stopped */
  telemt: { running: boolean; restarts: number; uptimeSec: number; lastExit: string; crashLoop: boolean; held?: string }
  capture: { rules: boolean; active: number; rejected: number }
  cover: { domain: string; ok: boolean; checkedAt: string | null; error: string }
}

export interface ServerEvent {
  at: string
  kind: string
  detail: string
}

export type LogLevel = 'silent' | 'normal' | 'verbose' | 'debug'

export interface ProxyOptions {
  extraCoverNames: string[]
  /** 0: the proxy's default */
  deviceWindowSecs: number
  logLevel: LogLevel
}

export interface ServerUsage {
  /** the agent reports connections and devices, and the numbers are fresh */
  live: boolean
  usersOnline: number
  connections: number
  devices: number
  /** bytes/s client to Telegram, and back */
  rateUp: number
  rateDown: number
  todayUp: number
  todayDown: number
  monthUp: number
  monthDown: number
  updatedAt: string | null
}

export interface Server {
  id: number
  name: string
  host: string
  sshPort: number
  sshUser: string
  hostKey: string
  /** False when the install died before it was saved: no key or link, so only removal works. */
  registered: boolean
  createdAt: string
  coverDomain: string
  maxStreams: number
  hubPort: number
  arch: string
  agentVersion: string
  telemtVersion: string
  lastSeenAt: string | null
  userCount: number
  link: LinkState
  agent: AgentState | null
  sync: {
    desiredRev: string
    appliedRev: string
    lastPushAt: string | null
    lastPushError: string
    healthy: boolean
  }
  events?: ServerEvent[]
  /** stopped by the user; stays off across restarts */
  proxyStopped: boolean
  proxyOptions: ProxyOptions
  updates: { agent: boolean; telemt: boolean; agentVersion: string; telemtVersion: string }
  usage: ServerUsage
}

export interface CoverCheck {
  ok: boolean
  tls13: boolean
  x25519: boolean
  h2: boolean
  certOk: boolean
  addrs: string[]
  latencyMs: number
  error: string
}

export interface Links {
  tg: string
  https: string
}

export interface User {
  name: string
  enabled: boolean
  state: 'active' | 'disabled' | 'expired' | 'over-quota'
  maxIps: number
  expiresAt: string | null
  quotaBytes: number
  up: number
  down: number
  createdAt: string
  secret: string
  note: string
  /** null: the agent can't say, or the numbers are stale */
  online: boolean | null
  connections: number | null
  devices: number | null
  lastSeenAt: string | null
  todayUp: number
  todayDown: number
  monthUp: number
  monthDown: number
  links: Links & { extra: (Links & { domain: string })[] }
}

export interface CreateUser {
  name: string
  enabled?: boolean
  maxIps?: number
  expiresAt?: string | null
  quotaBytes?: number
  note?: string
  secret?: string
}

export type UpdateUser = Omit<CreateUser, 'name' | 'secret'>

export type BulkAction = 'pause' | 'resume' | 'renew' | 'delete'

export interface Traffic {
  stepSec: number
  samples: { at: string; up: number; down: number }[]
}

type Started = { jobId: string }

// Local MTProto has one proxy, at api/local; its routes below it match a server's
const srv = (id: number) => (__LOCAL__ ? 'local' : `servers/${id}`)

export const api = {
  config: () => request<AppConfig>('GET', 'config'),
  probe: (req: ProbeRequest) => request<ProbeResult>('POST', 'probe', req),
  install: (req: InstallRequest) => request<Started>('POST', 'servers', req),
  job: (jobId: string) => request<Job>('GET', `jobs/${encodeURIComponent(jobId)}`),
  jobs: () => request<Job[]>('GET', 'jobs'),
  servers: () => request<Server[]>('GET', 'servers'),
  server: (id: number) => request<Server>('GET', srv(id)),
  rename: (id: number, name: string) => request<Server>('PATCH', srv(id), { name }),
  settings: (id: number, s: { coverDomain?: string; maxStreams?: number; proxyOptions?: Partial<ProxyOptions> }) =>
    request<Server>('PATCH', `${srv(id)}/settings`, s),
  repair: (id: number, req: RepairRequest) => request<Started>('POST', `${srv(id)}/repair`, req),
  remove: (id: number, req: RemoveRequest = {}) =>
    request<Started>('POST', `${srv(id)}/remove`, req),
  update: (id: number, kind: 'agent' | 'telemt') =>
    request<Started>('POST', `${srv(id)}/update`, { kind }),
  rotate: (id: number) => request<Started>('POST', `${srv(id)}/rotate`),
  restart: (id: number) => request<{ ok: boolean }>('POST', `${srv(id)}/restart`),
  stopProxy: (id: number, stopped: boolean) => request<Server>('POST', `${srv(id)}/proxy`, { stopped }),
  linkPort: (id: number, port: number) => request<Started>('POST', `${srv(id)}/link-port`, { port }),
  checkCover: (id: number, domain: string) =>
    request<CoverCheck>('POST', `${srv(id)}/disguise-check`, { domain }),
  logs: (id: number, lines = 300) =>
    request<{ lines: string[] }>('GET', `${srv(id)}/logs?lines=${lines}`),
  traffic: (id: number) => request<Traffic>('GET', `${srv(id)}/traffic`),
  refreshAllowlist: () => request<Allowlist>('POST', 'allowlist/refresh'),
}

const user = (id: number, name: string) => `${srv(id)}/users/${encodeURIComponent(name)}`

export const users = {
  list: (id: number) => request<User[]>('GET', `${srv(id)}/users`),
  create: (id: number, u: CreateUser) => request<User>('POST', `${srv(id)}/users`, u),
  update: (id: number, name: string, u: UpdateUser) => request<User>('PATCH', user(id, name), u),
  remove: (id: number, name: string) => request<{ ok: boolean }>('DELETE', user(id, name)),
  rotateSecret: (id: number, name: string) =>
    request<User>('POST', `${user(id, name)}/rotate-secret`),
  resetUsage: (id: number, name: string) => request<User>('POST', `${user(id, name)}/reset-usage`),
  bulk: (id: number, action: BulkAction, names: string[], days?: number) =>
    request<{ changed: number }>('POST', `${srv(id)}/users/bulk`, { action, names, ...(days ? { days } : {}) }),
}

// --- Local MTProto --------------------------------------------------------------

/** Where links point, and what the router script last said. */
export interface LocalAddress {
  /** what links use; "" while it isn't known */
  host: string
  /** what links use; 0 while it isn't known */
  port: number
  customHost: string
  /** 0: the router's own port */
  customPort: number
  routerPort: number
  cloudName: string
  publicAddress: string
  /** the domestic link's address on the router */
  wanAddress: string
  cloudStatus: string
  domestic: boolean
  /** the router script has written its state */
  reported: boolean
  reportedAt: string | null
  /** a new port the router hasn't taken yet */
  newPortAskedAt: string | null
}

export interface LocalProxy {
  version: string
  arch: string
  createdAt: string
  /** "" until the first-run setup */
  coverDomain: string
  userCount: number
  proxyStopped: boolean
  proxyOptions: ProxyOptions
  maxConns: { value: number; default: number; min: number; max: number }
  proxy: {
    running: boolean
    restarts: number
    uptimeSec: number
    lastExit: string
    crashLoop: boolean
    /** why it is kept off */
    held: string
    applyError: string
    version: string
    /** how telemt was built: "" for telemt's own release */
    build: string
    rssBytes: number
    port: number
    reportedAt: string
  }
  cover: { domain: string; ok: boolean; checkedAt: string | null; error: string }
  address: LocalAddress
  usage: ServerUsage
  events: ServerEvent[]
  uptimeSec: number
  rssBytes: number
}

export interface LocalSettings {
  coverDomain?: string
  maxConns?: number
  proxyOptions?: Partial<ProxyOptions>
  linkHost?: string
  linkPort?: number
}

export const local = {
  get: () => request<LocalProxy>('GET', 'local'),
  setup: (coverDomain: string, firstUser: string) => request<LocalProxy>('POST', 'local/setup', { coverDomain, firstUser }),
  settings: (s: LocalSettings) => request<LocalProxy>('PATCH', 'local/settings', s),
  newPort: () => request<LocalProxy>('POST', 'local/new-port'),
}

/** The proxy as the screens shared with MTProto Remote read a server. */
export function asServer(p: LocalProxy): Server {
  const t = p.proxy
  return {
    id: 1,
    name: 'Local MTProto',
    host: p.address.host,
    sshPort: 0,
    sshUser: '',
    hostKey: '',
    registered: true,
    createdAt: p.createdAt,
    coverDomain: p.coverDomain,
    maxStreams: p.maxConns.value,
    hubPort: 0,
    arch: p.arch,
    agentVersion: p.version,
    telemtVersion: t.version,
    lastSeenAt: null,
    userCount: p.userCount,
    link: { state: 'up', up: 1, total: 1, streams: 0, relayed: 0, dialFailures: 0, violations: 0, bytesToDc: 0, bytesFromDc: 0, conns: [] },
    agent: {
      version: p.version,
      telemtVersion: t.version,
      uptimeSec: p.uptimeSec,
      rssBytes: p.rssBytes,
      stateRev: '',
      reportedAt: t.reportedAt,
      telemt: { running: t.running, restarts: t.restarts, uptimeSec: t.uptimeSec, lastExit: t.lastExit, crashLoop: t.crashLoop, held: t.held },
      capture: { rules: true, active: 0, rejected: 0 },
      cover: p.cover,
    },
    sync: { desiredRev: '', appliedRev: '', lastPushAt: null, lastPushError: t.applyError, healthy: !t.applyError },
    events: p.events,
    proxyStopped: p.proxyStopped,
    proxyOptions: p.proxyOptions,
    updates: { agent: false, telemt: false, agentVersion: '', telemtVersion: '' },
    usage: p.usage,
  }
}

/**
 * Follows a job until it ends: over its event stream, or by polling when the
 * stream fails or stays silent. Returns a function that stops following.
 */
export function followJob(
  jobId: string,
  onUpdate: (job: Job) => void,
  { pollMs = 1000, silentMs = 5000 } = {},
): () => void {
  let stopped = false
  let polling = false
  let last: Job | null = null
  let source: EventSource | null = null
  let timer: ReturnType<typeof setTimeout> | undefined
  let watchdog: ReturnType<typeof setTimeout> | undefined

  const deliver = (job: Job) => {
    if (stopped) return
    last = job
    onUpdate(job)
    if (job.state !== 'running') stop()
  }

  const poll = async () => {
    if (stopped) return
    try {
      deliver(await api.job(jobId))
    } catch (e) {
      // a restarted plugin forgets its jobs, so this one will never end
      if (e instanceof ApiError && e.status === 404) {
        return deliver({
          ...(last ?? { id: jobId, kind: 'install', steps: [], startedAt: '' }),
          state: 'failed',
          error: 'The router lost track of this job, likely after a restart. Check the server, then try again.',
        })
      }
      // anything else is a blip; the next tick tries again
    }
    if (!stopped) timer = setTimeout(poll, pollMs)
  }

  const fallBack = () => {
    clearTimeout(watchdog)
    source?.close()
    source = null
    if (stopped || polling) return
    polling = true
    void poll()
  }

  const stop = () => {
    stopped = true
    clearTimeout(timer)
    clearTimeout(watchdog)
    source?.close()
    source = null
  }

  if (typeof EventSource === 'undefined') {
    fallBack()
  } else {
    source = new EventSource(`api/jobs/${encodeURIComponent(jobId)}/stream`)
    source.onmessage = (e) => {
      clearTimeout(watchdog)
      let job: Job
      try {
        job = JSON.parse(e.data)
      } catch {
        return fallBack()
      }
      deliver(job)
    }
    // the stream closes after the final state, which deliver() has already seen
    source.onerror = fallBack
    // a proxy that buffers the stream would show nothing until the job ends
    watchdog = setTimeout(fallBack, silentMs)
  }
  return stop
}
