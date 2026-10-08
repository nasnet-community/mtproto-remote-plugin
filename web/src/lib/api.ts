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
  state: JobState
  detail?: string
}

export interface Job {
  id: string
  kind: 'install' | 'repair' | 'uninstall' | 'update-agent' | 'update-telemt' | 'rotate'
  state: JobState
  steps: JobStep[]
  error?: string
  serverId?: number
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
  links: { tg: string; https: string }
}

export interface CreateUser {
  name: string
  enabled?: boolean
  maxIps?: number
  expiresAt?: string | null
  quotaBytes?: number
}

export type UpdateUser = Omit<CreateUser, 'name'>

type Started = { jobId: string }

const srv = (id: number) => `servers/${id}`

export const api = {
  config: () => request<AppConfig>('GET', 'config'),
  probe: (req: ProbeRequest) => request<ProbeResult>('POST', 'probe', req),
  install: (req: InstallRequest) => request<Started>('POST', 'servers', req),
  job: (jobId: string) => request<Job>('GET', `jobs/${encodeURIComponent(jobId)}`),
  servers: () => request<Server[]>('GET', 'servers'),
  server: (id: number) => request<Server>('GET', srv(id)),
  rename: (id: number, name: string) => request<Server>('PATCH', srv(id), { name }),
  settings: (id: number, s: { coverDomain?: string; maxStreams?: number }) =>
    request<Server>('PATCH', `${srv(id)}/settings`, s),
  repair: (id: number, req: RepairRequest) => request<Started>('POST', `${srv(id)}/repair`, req),
  remove: (id: number, req: RemoveRequest = {}) =>
    request<Started>('POST', `${srv(id)}/remove`, req),
  update: (id: number, kind: 'agent' | 'telemt') =>
    request<Started>('POST', `${srv(id)}/update`, { kind }),
  rotate: (id: number) => request<Started>('POST', `${srv(id)}/rotate`),
  restart: (id: number) => request<{ ok: boolean }>('POST', `${srv(id)}/restart`),
  checkCover: (id: number, domain: string) =>
    request<CoverCheck>('POST', `${srv(id)}/disguise-check`, { domain }),
  logs: (id: number, lines = 300) =>
    request<{ lines: string[] }>('GET', `${srv(id)}/logs?lines=${lines}`),
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
