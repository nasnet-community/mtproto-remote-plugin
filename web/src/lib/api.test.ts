import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api, followJob, users, type Job } from './api'

const job = (state: Job['state'], steps: Job['steps'] = []): Job => ({
  id: 'j1',
  kind: 'install',
  state,
  steps,
  startedAt: '2026-10-01T00:00:00Z',
})

const reply = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

class FakeEventSource {
  static last: FakeEventSource | null = null
  onmessage: ((e: { data: string }) => void) | null = null
  onerror: (() => void) | null = null
  closed = false
  constructor(readonly url: string) {
    FakeEventSource.last = this
  }
  close() {
    this.closed = true
  }
  send(j: Job) {
    this.onmessage?.({ data: JSON.stringify(j) })
  }
}

let fetchMock: ReturnType<typeof vi.fn>

beforeEach(() => {
  fetchMock = vi.fn()
  vi.stubGlobal('fetch', fetchMock)
  FakeEventSource.last = null
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

describe('request', () => {
  it('calls relative URLs with a JSON body', async () => {
    fetchMock.mockResolvedValue(reply(202, { jobId: 'abc' }))
    await expect(api.update(3, 'agent')).resolves.toEqual({ jobId: 'abc' })
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('api/servers/3/update')
    expect(init.method).toBe('POST')
    expect(JSON.parse(init.body)).toEqual({ kind: 'agent' })
  })

  it('marks every call as the page\'s own, with or without a body', async () => {
    fetchMock.mockResolvedValue(reply(200, {}))
    await api.servers()
    await api.rename(1, 'x')
    for (const [, init] of fetchMock.mock.calls) expect(init.headers['X-MTProto-UI']).toBe('1')
    expect(fetchMock.mock.calls[1][1].headers['Content-Type']).toBe('application/json')
  })

  it('escapes user names in paths', async () => {
    fetchMock.mockResolvedValue(reply(200, {}))
    await users.resetUsage(1, 'a b/c')
    expect(fetchMock.mock.calls[0][0]).toBe('api/servers/1/users/a%20b%2Fc/reset-usage')
  })

  it("turns an error body into an ApiError and keeps the server's extra fields", async () => {
    fetchMock.mockResolvedValue(reply(409, { error: 'log in first', needLogin: true }))
    const err = await api.remove(2).catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err.message).toBe('log in first')
    expect(err.status).toBe(409)
    expect(err.needLogin).toBe(true)
  })

  it('names the status when the body has no message', async () => {
    fetchMock.mockResolvedValue(new Response('bad gateway', { status: 502 }))
    const err = await api.servers().catch((e) => e)
    expect(err.message).toBe('Request failed (502)')
    expect(err.needLogin).toBe(false)
  })
})

describe('followJob', () => {
  it('follows the event stream and stops at the final state', () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    const seen: Job['state'][] = []
    followJob('j 1', (j) => seen.push(j.state))
    const es = FakeEventSource.last!
    expect(es.url).toBe('api/jobs/j%201/stream')
    es.send(job('running'))
    es.send(job('running', [{ name: 'check', state: 'ok' }]))
    es.send(job('failed'))
    expect(seen).toEqual(['running', 'running', 'failed'])
    expect(es.closed).toBe(true)
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('polls when the stream errors', async () => {
    vi.useFakeTimers()
    vi.stubGlobal('EventSource', FakeEventSource)
    fetchMock
      .mockResolvedValueOnce(reply(200, job('running')))
      .mockResolvedValueOnce(reply(200, job('ok')))
    const seen: Job['state'][] = []
    followJob('j1', (j) => seen.push(j.state), { pollMs: 100 })
    FakeEventSource.last!.onerror?.()
    expect(FakeEventSource.last!.closed).toBe(true)
    await vi.advanceTimersByTimeAsync(250)
    expect(fetchMock.mock.calls.map((c) => c[0])).toEqual(['api/jobs/j1', 'api/jobs/j1'])
    expect(seen).toEqual(['running', 'ok'])
    await vi.advanceTimersByTimeAsync(1000)
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  it('polls when the stream stays silent, as behind a buffering proxy', async () => {
    vi.useFakeTimers()
    vi.stubGlobal('EventSource', FakeEventSource)
    fetchMock.mockResolvedValue(reply(200, job('ok')))
    const seen: Job['state'][] = []
    followJob('j1', (j) => seen.push(j.state), { silentMs: 500 })
    await vi.advanceTimersByTimeAsync(400)
    expect(fetchMock).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(200)
    expect(seen).toEqual(['ok'])
    expect(FakeEventSource.last!.closed).toBe(true)
  })

  it('polls without EventSource and stops when asked', async () => {
    vi.useFakeTimers()
    vi.stubGlobal('EventSource', undefined)
    fetchMock.mockImplementation(async () => reply(200, job('running')))
    const stop = followJob('j1', () => {}, { pollMs: 100 })
    await vi.advanceTimersByTimeAsync(250)
    const calls = fetchMock.mock.calls.length
    expect(calls).toBeGreaterThanOrEqual(2)
    stop()
    await vi.advanceTimersByTimeAsync(1000)
    expect(fetchMock.mock.calls.length).toBe(calls)
  })

  it('fails a job the router no longer knows', async () => {
    vi.useFakeTimers()
    vi.stubGlobal('EventSource', FakeEventSource)
    fetchMock.mockResolvedValue(reply(404, { error: 'no such job' }))
    const seen: Job[] = []
    followJob('j1', (j) => seen.push(j), { pollMs: 100 })
    FakeEventSource.last!.send(job('running', [{ name: 'check', state: 'running' }]))
    FakeEventSource.last!.onerror?.()
    await vi.advanceTimersByTimeAsync(500)
    expect(fetchMock).toHaveBeenCalledTimes(1)
    const final = seen[seen.length - 1]
    expect(final.state).toBe('failed')
    expect(final.steps).toEqual([{ name: 'check', state: 'running' }])
    expect(final.error).toMatch(/lost track of this job/)
  })

  it('keeps polling through a failed poll', async () => {
    vi.useFakeTimers()
    vi.stubGlobal('EventSource', undefined)
    fetchMock
      .mockRejectedValueOnce(new TypeError('network down'))
      .mockResolvedValueOnce(reply(200, job('ok')))
    const seen: Job['state'][] = []
    followJob('j1', (j) => seen.push(j.state), { pollMs: 100 })
    await vi.advanceTimersByTimeAsync(250)
    expect(seen).toEqual(['ok'])
  })
})
