// Typed calls to the plugin's API. URLs are relative, so they resolve against
// the <base href> the server injects (the panel's plugin path).

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message)
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`api/${path}`, {
    method,
    headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) {
    const message =
      data?.error?.message ?? (typeof data?.error === 'string' ? data.error : undefined)
    throw new ApiError(message ?? `Request failed (${res.status})`, res.status)
  }
  return data as T
}

export interface AppConfig {
  version: string
  repo: string
  releaseTag: string
  routerKey: string
}

export interface Server {
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

export interface Step {
  title: string
  state: 'running' | 'done' | 'failed'
  detail: string
}

export interface Job {
  id: number
  running: boolean
  steps: Step[]
  error: string | null
  report: { serverId: string; release: string; keptConfig: boolean; users: number } | null
}

export interface ProbeResult {
  fingerprint: string
  status: 'new' | 'known' | 'changed'
}

export interface InstallRequest {
  host: string
  sshPort: number
  username: string
  password: string
  fingerprint: string
  name: string
  proxyPort: number
  tlsDomain: string
  firstUser: string
}

export interface ServerStatus {
  service: string
  info: { version?: string; uptime_seconds?: number } | null
  summary: { connections_total?: number; configured_users?: number } | null
  apiError: string | null
}

/** A user as telemt's API reports it. */
export interface TelemtUser {
  username: string
  enabled: boolean
  in_runtime: boolean
  max_tcp_conns: number | null
  expiration_rfc3339: string | null
  data_quota_bytes: number | null
  rate_limit_up_bps: number | null
  rate_limit_down_bps: number | null
  max_unique_ips: number | null
  current_connections: number
  active_unique_ips: number
  active_unique_ips_list: string[]
  recent_unique_ips: number
  total_octets: number
  links: { classic: string[]; secure: string[]; tls: string[] }
}

/** Limits a user can have; `null` clears one. */
export interface UserLimits {
  max_tcp_conns?: number | null
  max_unique_ips?: number | null
  data_quota_bytes?: number | null
  expiration_rfc3339?: string | null
  rate_limit_up_bps?: number | null
  rate_limit_down_bps?: number | null
  enabled?: boolean
}

export const api = {
  config: () => request<AppConfig>('GET', 'config'),
  probe: (host: string, sshPort: number) =>
    request<ProbeResult>('POST', 'probe', { host, sshPort }),
  install: (req: InstallRequest) => request<{ id: number }>('POST', 'deploy', req),
  job: () => request<Job>('GET', 'job'),
  servers: () => request<Server[]>('GET', 'servers'),
  rename: (id: string, name: string) => request<Server>('PATCH', `servers/${id}`, { name }),
  remove: (id: string, uninstall: boolean) =>
    request<{ removed: boolean; warning: string | null }>('DELETE', `servers/${id}`, {
      uninstall,
    }),
  status: (id: string) => request<ServerStatus>('GET', `servers/${id}/status`),
  service: (id: string, action: 'start' | 'stop' | 'restart') =>
    request<{ ok: boolean }>('POST', `servers/${id}/service/${action}`),
  logs: (id: string, lines = 300) =>
    request<{ logs: string }>('GET', `servers/${id}/logs?lines=${lines}`),
  update: (id: string) => request<{ release: string }>('POST', `servers/${id}/update`),
}

/** Calls telemt's API on a server and unwraps its `{ ok, data }` envelope. */
async function telemt<T>(id: string, method: string, path: string, body?: unknown): Promise<T> {
  const res = await request<{ ok: boolean; data: T }>(
    method,
    `servers/${id}/telemt/v1/${path}`,
    body,
  )
  return res.data
}

const user = (name: string) => `users/${encodeURIComponent(name)}`

export const users = {
  list: (id: string) => telemt<TelemtUser[]>(id, 'GET', 'users'),
  create: (id: string, username: string, limits: UserLimits) =>
    telemt<{ user: TelemtUser; secret: string }>(id, 'POST', 'users', { username, ...limits }),
  update: (id: string, name: string, limits: UserLimits) =>
    telemt<TelemtUser>(id, 'PATCH', user(name), limits),
  remove: (id: string, name: string) => telemt<unknown>(id, 'DELETE', user(name)),
  setEnabled: (id: string, name: string, enabled: boolean) =>
    telemt<TelemtUser>(id, 'POST', `${user(name)}/${enabled ? 'enable' : 'disable'}`),
  rotateSecret: (id: string, name: string) =>
    telemt<{ user: TelemtUser; secret: string }>(id, 'POST', `${user(name)}/rotate-secret`),
  resetQuota: (id: string, name: string) =>
    telemt<unknown>(id, 'POST', `${user(name)}/reset-quota`),
  setTlsDomain: (id: string, domain: string) =>
    telemt<unknown>(id, 'PATCH', 'config', { censorship: { tls_domain: domain } }),
}
