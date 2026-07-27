import { describe, expect, it } from 'vitest'
import {
  ALL_TIME_START,
  DEFAULT_PRESET,
  addDays,
  calendarDayIn,
  matchPreset,
  normaliseZone,
  parseDayInput,
  presetRange,
  rangeFromParams,
  rangeLabel,
  startOfDayIn,
  toDayInput,
  zoneOffsetMs,
} from './range'

const NOW = new Date('2026-07-26T09:15:00Z')

describe('normaliseZone', () => {
  it('keeps a zone the browser knows', () => {
    expect(normaliseZone('Europe/Berlin')).toBe('Europe/Berlin')
  })

  it('falls back to UTC rather than throwing', () => {
    expect(normaliseZone('Mars/Olympus_Mons')).toBe('UTC')
    expect(normaliseZone('')).toBe('UTC')
    expect(normaliseZone(null)).toBe('UTC')
  })
})

describe('zone arithmetic', () => {
  it('reports the offset in force at the instant, not the one in January', () => {
    // Berlin is UTC+1 in winter and UTC+2 under summer time.
    expect(zoneOffsetMs(new Date('2026-01-15T12:00:00Z'), 'Europe/Berlin')).toBe(3600_000)
    expect(zoneOffsetMs(new Date('2026-07-15T12:00:00Z'), 'Europe/Berlin')).toBe(7200_000)
  })

  it('finds the calendar day a moment falls on locally', () => {
    expect(calendarDayIn(new Date('2026-07-26T22:30:00Z'), 'Asia/Tokyo')).toEqual({
      year: 2026,
      month: 7,
      day: 27,
    })
  })

  it('starts a day at local midnight', () => {
    const start = startOfDayIn({ year: 2026, month: 7, day: 26 }, 'Europe/Berlin')
    expect(start.toISOString()).toBe('2026-07-25T22:00:00.000Z')
  })

  it('starts a day correctly across a daylight-saving change', () => {
    // Clocks go forward in Berlin on 29 March 2026; the day still begins at
    // local midnight, which is 23:00 UTC the evening before.
    const start = startOfDayIn({ year: 2026, month: 3, day: 29 }, 'Europe/Berlin')
    expect(start.toISOString()).toBe('2026-03-28T23:00:00.000Z')
  })

  it('rolls months and years when shifting days', () => {
    expect(addDays({ year: 2026, month: 1, day: 1 }, -1)).toEqual({
      year: 2025,
      month: 12,
      day: 31,
    })
    expect(addDays({ year: 2024, month: 2, day: 28 }, 1)).toEqual({ year: 2024, month: 2, day: 29 })
  })

  it('round-trips a date input value', () => {
    expect(toDayInput({ year: 2026, month: 7, day: 6 })).toBe('2026-07-06')
    expect(parseDayInput('2026-07-06')).toEqual({ year: 2026, month: 7, day: 6 })
    expect(parseDayInput('26/07/2026')).toBeNull()
    expect(parseDayInput('2026-13-01')).toBeNull()
  })
})

describe('presetRange', () => {
  it('ends at the start of tomorrow so today counts in full', () => {
    const range = presetRange('7d', 'UTC', NOW)
    expect(range.to).toBe('2026-07-27T00:00:00.000Z')
    expect(range.from).toBe('2026-07-20T00:00:00.000Z')
  })

  it('matches the API default for the trailing thirty days', () => {
    const range = presetRange('30d', 'UTC', NOW)
    expect(range.from).toBe('2026-06-27T00:00:00.000Z')
    expect(range.to).toBe('2026-07-27T00:00:00.000Z')
  })

  it('starts this year at local midnight on 1 January', () => {
    const range = presetRange('ytd', 'Europe/Berlin', NOW)
    expect(range.from).toBe('2025-12-31T23:00:00.000Z')
  })

  it('bounds all time rather than leaving it open', () => {
    expect(presetRange('all', 'UTC', NOW).from).toBe(ALL_TIME_START)
  })

  it('starts "all time" at the first listen when one is known', () => {
    const first = '2019-03-04T12:00:00.000Z'
    expect(presetRange('all', 'UTC', NOW, first).from).toBe(first)
  })

  it('falls back to the floor when the first listen is not known yet', () => {
    // A user with no history at all, or a session that has not loaded. Drawing
    // from the floor is harmless here because there is nothing to plot.
    expect(presetRange('all', 'UTC', NOW, '').from).toBe(ALL_TIME_START)
  })

  it('still recognises an all-time range built from a first listen', () => {
    const first = '2019-03-04T12:00:00.000Z'
    const range = presetRange('all', 'UTC', NOW, first)
    expect(matchPreset(range, 'UTC', NOW, first)).toBe('all')
  })

  it('aligns boundaries to the user’s timezone, not the browser’s', () => {
    expect(presetRange('7d', 'Asia/Tokyo', NOW).to).toBe('2026-07-26T15:00:00.000Z')
  })
})

describe('matchPreset', () => {
  it('recognises a range it produced', () => {
    for (const id of ['7d', '30d', '90d', 'ytd', 'all'] as const) {
      expect(matchPreset(presetRange(id, 'UTC', NOW), 'UTC', NOW)).toBe(id)
    }
  })

  it('calls anything else custom', () => {
    const custom = { from: '2026-01-05T00:00:00.000Z', to: '2026-02-05T00:00:00.000Z' }
    expect(matchPreset(custom, 'UTC', NOW)).toBe('custom')
  })
})

describe('rangeFromParams', () => {
  it('reads a range out of the query string', () => {
    const params = new URLSearchParams({
      from: '2026-01-01T00:00:00Z',
      to: '2026-02-01T00:00:00Z',
    })
    expect(rangeFromParams(params, 'UTC', NOW)).toEqual({
      from: '2026-01-01T00:00:00.000Z',
      to: '2026-02-01T00:00:00.000Z',
    })
  })

  it('falls back to the default preset when the URL says nothing', () => {
    expect(rangeFromParams(new URLSearchParams(), 'UTC', NOW)).toEqual(
      presetRange(DEFAULT_PRESET, 'UTC', NOW),
    )
  })

  it('falls back rather than failing on a hand-edited URL', () => {
    const half = new URLSearchParams({ from: '2026-01-01T00:00:00Z' })
    expect(rangeFromParams(half, 'UTC', NOW)).toEqual(presetRange(DEFAULT_PRESET, 'UTC', NOW))

    const reversed = new URLSearchParams({
      from: '2026-02-01T00:00:00Z',
      to: '2026-01-01T00:00:00Z',
    })
    expect(rangeFromParams(reversed, 'UTC', NOW)).toEqual(presetRange(DEFAULT_PRESET, 'UTC', NOW))

    const nonsense = new URLSearchParams({ from: 'yesterday', to: 'today' })
    expect(rangeFromParams(nonsense, 'UTC', NOW)).toEqual(presetRange(DEFAULT_PRESET, 'UTC', NOW))
  })
})

describe('rangeLabel', () => {
  it('names a preset', () => {
    expect(rangeLabel(presetRange('90d', 'UTC', NOW), 'UTC', NOW)).toBe('Last 90 days')
  })

  it('shows a custom range as inclusive days', () => {
    const custom = { from: '2026-01-05T00:00:00.000Z', to: '2026-01-08T00:00:00.000Z' }
    expect(rangeLabel(custom, 'UTC', NOW)).toBe('2026-01-05 to 2026-01-07')
  })
})
