import { describe, expect, it } from 'vitest'
import { parseRoute } from './useRoute'

describe('parseRoute', () => {
  it('maps hashes to pages', () => {
    expect(parseRoute('')).toEqual({ page: 'servers' })
    expect(parseRoute('#/')).toEqual({ page: 'servers' })
    expect(parseRoute('#/servers/3')).toEqual({ page: 'server', id: '3', tab: 'users' })
    for (const tab of ['status', 'settings', 'logs']) {
      expect(parseRoute(`#/servers/3/${tab}`)).toEqual({ page: 'server', id: '3', tab })
    }
    expect(parseRoute('#/servers/3/nonsense')).toEqual({ page: 'server', id: '3', tab: 'users' })
    expect(parseRoute('#/nonsense')).toEqual({ page: 'servers' })
  })
})
