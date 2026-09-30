import { describe, expect, it } from 'vitest'
import { parseRoute } from './useRoute'

describe('parseRoute', () => {
  it('maps hashes to pages', () => {
    expect(parseRoute('')).toEqual({ page: 'servers' })
    expect(parseRoute('#/')).toEqual({ page: 'servers' })
    expect(parseRoute('#/servers/ab12')).toEqual({ page: 'server', id: 'ab12', tab: 'users' })
    expect(parseRoute('#/servers/ab12/settings')).toEqual({
      page: 'server',
      id: 'ab12',
      tab: 'settings',
    })
    expect(parseRoute('#/nonsense')).toEqual({ page: 'servers' })
  })
})
