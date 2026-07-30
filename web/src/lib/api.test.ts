import { describe, expect, it } from 'vitest'
import { buildQuery } from './api'

describe('buildQuery', () => {
  it('returns nothing for an absent or empty query', () => {
    expect(buildQuery(undefined)).toBe('')
    expect(buildQuery({})).toBe('')
  })

  it('serialises scalars', () => {
    expect(buildQuery({ from: '2026-01-01', limit: 50, active: true })).toBe(
      '?from=2026-01-01&limit=50&active=true',
    )
  })

  it('drops null, undefined and empty-string values rather than emitting them', () => {
    expect(buildQuery({ from: null, to: undefined, q: '', limit: 10 })).toBe('?limit=10')
  })

  it('repeats an array value as one parameter per element, not as one joined value', () => {
    // The server reads r.URL.Query()["genre"], which needs repetition rather
    // than a single comma-joined string.
    expect(buildQuery({ genre: ['rock', 'jazz'] })).toBe('?genre=rock&genre=jazz')
  })

  it('omits an array key entirely when the array is empty', () => {
    expect(buildQuery({ genre: [], limit: 10 })).toBe('?limit=10')
  })
})
