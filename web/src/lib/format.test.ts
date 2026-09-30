import { describe, expect, it } from 'vitest'
import {
  GB,
  MBIT,
  formatBytes,
  formatDuration,
  limitText,
  parseLimit,
  relativeTime,
  toWebLink,
} from './format'

describe('format', () => {
  it('formats bytes with sensible precision', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(999)).toBe('999 B')
    expect(formatBytes(1_500)).toBe('1.50 KB')
    expect(formatBytes(12_345_678)).toBe('12.3 MB')
    expect(formatBytes(5 * GB)).toBe('5.00 GB')
  })

  it('formats durations', () => {
    expect(formatDuration(42)).toBe('42s')
    expect(formatDuration(3_720)).toBe('1h 2m')
    expect(formatDuration(90_000)).toBe('1d 1h')
  })

  it('describes times relative to now', () => {
    const now = Date.parse('2026-09-29T12:00:00Z')
    expect(relativeTime('2026-10-02T12:00:00Z', now)).toBe('in 3 days')
    expect(relativeTime('2026-09-29T10:00:00Z', now)).toBe('2 hours ago')
    expect(relativeTime('2026-09-29T12:00:20Z', now)).toBe('in 1 minute')
  })

  it('parses optional limits', () => {
    expect(parseLimit('')).toBeNull()
    expect(parseLimit(' 5 ', GB)).toBe(5 * GB)
    expect(parseLimit('1.5', MBIT)).toBe(1_500_000)
    expect(parseLimit('0')).toBe('invalid')
    expect(parseLimit('abc')).toBe('invalid')
    expect(limitText(null)).toBe('')
    expect(limitText(5 * GB, GB)).toBe('5')
    expect(limitText(1_500_000, MBIT)).toBe('1.5')
  })

  it('turns tg:// links into t.me links', () => {
    expect(toWebLink('tg://proxy?server=a&port=443&secret=ee00')).toBe(
      'https://t.me/proxy?server=a&port=443&secret=ee00',
    )
  })
})
