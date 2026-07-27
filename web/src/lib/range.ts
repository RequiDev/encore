/**
 * The shared date range.
 *
 * The range lives in the URL query string, not in React state, so any view a
 * person is looking at can be copied out of the address bar and sent to someone
 * else — or bookmarked — and come back showing the same thing. `from` and `to`
 * are RFC 3339 instants, exactly the form the API takes, so the value read out
 * of the URL is passed straight through without a second representation to keep
 * in step.
 *
 * All preset boundaries are computed in the *user's* timezone, because that is
 * the timezone the server buckets in. "Last 7 days" has to start at local
 * midnight or the first bucket of every chart would be a partial day.
 */

import { useCallback, useMemo } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useListeningBounds, useTimeZone } from './session'

/** A half-open range `[from, to)`, as RFC 3339 instants. */
export interface DateRange {
  from: string
  to: string
}

export type RangePresetId = '7d' | '30d' | '90d' | 'ytd' | 'all'

export interface RangePreset {
  id: RangePresetId
  label: string
  /** Announced in full where the two-character label would be cryptic. */
  description: string
}

export const RANGE_PRESETS: readonly RangePreset[] = [
  { id: '7d', label: '7d', description: 'Last 7 days' },
  { id: '30d', label: '30d', description: 'Last 30 days' },
  { id: '90d', label: '90d', description: 'Last 90 days' },
  { id: 'ytd', label: 'Year', description: 'This year' },
  { id: 'all', label: 'All', description: 'All time' },
]

/** The preset used when the URL carries no range, matching the API's own default. */
export const DEFAULT_PRESET: RangePresetId = '30d'

/**
 * The floor for "all time" when the caller does not know when the user's history
 * actually begins. Spotify launched in 2008 and its exports contain nothing
 * older, so this includes every listen anyone can have.
 *
 * It is a fallback, not the usual answer. Given a real first-listen instant,
 * prefer that: a chart drawn from this floor spends most of its width on years
 * the user did not exist on Spotify, and hovering there reports 2006 rather than
 * anything meaningful.
 */
export const ALL_TIME_START = '2006-01-01T00:00:00.000Z'

// --- timezone arithmetic ---------------------------------------------------

const DAY_MS = 86_400_000

const zoneCache = new Map<string, boolean>()

/**
 * Falls back to UTC for a timezone the browser does not know. The server
 * validates the value, but an instance restored from an old backup could carry
 * a zone this browser's ICU data has since renamed, and a thrown RangeError
 * inside a formatter would take the whole page down.
 */
export function normaliseZone(timeZone: string | null | undefined): string {
  if (!timeZone) return 'UTC'
  const cached = zoneCache.get(timeZone)
  if (cached !== undefined) return cached ? timeZone : 'UTC'
  try {
    new Intl.DateTimeFormat('en-GB', { timeZone }).format(0)
    zoneCache.set(timeZone, true)
    return timeZone
  } catch {
    zoneCache.set(timeZone, false)
    return 'UTC'
  }
}

/** A calendar date, as it reads on a wall clock in some timezone. */
export interface CalendarDay {
  year: number
  month: number
  day: number
}

const partsFormatters = new Map<string, Intl.DateTimeFormat>()

function partsFormatter(timeZone: string): Intl.DateTimeFormat {
  const cached = partsFormatters.get(timeZone)
  if (cached) return cached
  const made = new Intl.DateTimeFormat('en-GB', {
    timeZone,
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hourCycle: 'h23',
  })
  partsFormatters.set(timeZone, made)
  return made
}

function wallClockUtcMs(instant: Date, timeZone: string): number {
  const parts = partsFormatter(timeZone).formatToParts(instant)
  const read = (type: Intl.DateTimeFormatPartTypes): number =>
    Number(parts.find((p) => p.type === type)?.value ?? '0')
  // h23 renders midnight as 24 in some ICU versions; normalise it to 0.
  const hour = read('hour') % 24
  return Date.UTC(
    read('year'),
    read('month') - 1,
    read('day'),
    hour,
    read('minute'),
    read('second'),
  )
}

/** The zone's offset from UTC at a given instant, in milliseconds, east positive. */
export function zoneOffsetMs(instant: Date, timeZone: string): number {
  const seconds = Math.floor(instant.getTime() / 1000) * 1000
  return wallClockUtcMs(instant, timeZone) - seconds
}

/** The calendar day an instant falls on in the given timezone. */
export function calendarDayIn(instant: Date, timeZone: string): CalendarDay {
  const wall = wallClockUtcMs(instant, timeZone)
  const asUtc = new Date(wall)
  return {
    year: asUtc.getUTCFullYear(),
    month: asUtc.getUTCMonth() + 1,
    day: asUtc.getUTCDate(),
  }
}

/**
 * The instant at which a calendar day begins in the given timezone.
 *
 * The offset has to be looked up at the answer rather than at the guess, since
 * a day that starts either side of a daylight-saving transition has a different
 * offset from the one midnight UTC would suggest; one refinement is enough for
 * every real zone.
 */
export function startOfDayIn(day: CalendarDay, timeZone: string): Date {
  const guess = Date.UTC(day.year, day.month - 1, day.day)
  const first = zoneOffsetMs(new Date(guess), timeZone)
  const candidate = new Date(guess - first)
  const second = zoneOffsetMs(candidate, timeZone)
  return second === first ? candidate : new Date(guess - second)
}

/** Shifts a calendar day by whole days, rolling months and years correctly. */
export function addDays(day: CalendarDay, days: number): CalendarDay {
  const shifted = new Date(Date.UTC(day.year, day.month - 1, day.day) + days * DAY_MS)
  return {
    year: shifted.getUTCFullYear(),
    month: shifted.getUTCMonth() + 1,
    day: shifted.getUTCDate(),
  }
}

/** `2026-07-26` for a calendar day, the form an `<input type="date">` uses. */
export function toDayInput(day: CalendarDay): string {
  const pad = (n: number) => n.toString().padStart(2, '0')
  return `${day.year}-${pad(day.month)}-${pad(day.day)}`
}

/** Parses `2026-07-26`, returning null for anything else. */
export function parseDayInput(value: string): CalendarDay | null {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value.trim())
  if (!match) return null
  const [, y, m, d] = match
  const day = { year: Number(y), month: Number(m), day: Number(d) }
  if (day.month < 1 || day.month > 12 || day.day < 1 || day.day > 31) return null
  return day
}

// --- presets ---------------------------------------------------------------

/**
 * Resolves a preset into instants. `to` is the start of *tomorrow* local, so
 * today is included in full — the range is half-open, and a `to` of midnight
 * today would silently drop everything played since waking up.
 */
export function presetRange(
  id: RangePresetId,
  timeZone: string,
  now: Date = new Date(),
  /** When the user's history begins. Only "all" uses it. */
  allTimeStart: string = ALL_TIME_START,
): DateRange {
  const zone = normaliseZone(timeZone)
  const today = calendarDayIn(now, zone)
  const end = startOfDayIn(addDays(today, 1), zone)
  const to = end.toISOString()

  switch (id) {
    case '7d':
      return { from: startOfDayIn(addDays(today, -6), zone).toISOString(), to }
    case '90d':
      return { from: startOfDayIn(addDays(today, -89), zone).toISOString(), to }
    case 'ytd':
      return { from: startOfDayIn({ year: today.year, month: 1, day: 1 }, zone).toISOString(), to }
    case 'all':
      return { from: allTimeStart || ALL_TIME_START, to }
    case '30d':
    default:
      return { from: startOfDayIn(addDays(today, -29), zone).toISOString(), to }
  }
}

/**
 * Names the preset a range corresponds to, or `custom`. Matching on the exact
 * instants is enough because every preset is generated by the code above; a
 * hand-edited URL is custom, which is the honest answer.
 */
export function matchPreset(
  range: DateRange,
  timeZone: string,
  now: Date = new Date(),
  allTimeStart: string = ALL_TIME_START,
): RangePresetId | 'custom' {
  for (const preset of RANGE_PRESETS) {
    const candidate = presetRange(preset.id, timeZone, now, allTimeStart)
    if (candidate.from === range.from && candidate.to === range.to) return preset.id
  }
  return 'custom'
}

function isInstant(value: string | null): value is string {
  if (!value) return false
  const parsed = Date.parse(value)
  return Number.isFinite(parsed)
}

/**
 * Reads a range out of URL parameters, falling back to the default preset. A
 * half-specified or reversed range is treated as absent rather than as an
 * error: the address bar is a text field, and a person editing it by hand
 * should get a working page back.
 */
export function rangeFromParams(
  params: URLSearchParams,
  timeZone: string,
  now: Date = new Date(),
): DateRange {
  const from = params.get('from')
  const to = params.get('to')
  if (isInstant(from) && isInstant(to) && Date.parse(from) < Date.parse(to)) {
    return { from: new Date(from).toISOString(), to: new Date(to).toISOString() }
  }
  return presetRange(DEFAULT_PRESET, timeZone, now)
}

/** A short human label for a range, used in the picker and in page subtitles. */
export function rangeLabel(range: DateRange, timeZone: string, now: Date = new Date()): string {
  const preset = matchPreset(range, timeZone, now)
  if (preset !== 'custom') {
    return RANGE_PRESETS.find((p) => p.id === preset)?.description ?? 'Custom range'
  }
  const zone = normaliseZone(timeZone)
  const first = calendarDayIn(new Date(range.from), zone)
  // `to` is exclusive, so the last day a person sees is the instant before it.
  const last = calendarDayIn(new Date(new Date(range.to).getTime() - 1), zone)
  return `${toDayInput(first)} to ${toDayInput(last)}`
}

// --- the hook --------------------------------------------------------------

export interface RangeControls {
  /** Always resolved, never null, so callers pass it straight to the API. */
  range: DateRange
  /** Which preset the current range is, or `custom`. */
  preset: RangePresetId | 'custom'
  /** The user's timezone, already validated. */
  timeZone: string
  /** Human label for the current range. */
  label: string
  /** The first and last day a person sees, for the custom picker's inputs. */
  days: { from: string; to: string }
  setPreset: (id: RangePresetId) => void
  /** Both arguments are inclusive `yyyy-mm-dd` local days. */
  setCustomDays: (fromDay: string, toDay: string) => void
  /** Drops `from` and `to` from the URL, returning to the default. */
  reset: () => void
}

/**
 * The shared range, read from and written to the query string. Every other
 * search parameter on the page is preserved, so changing the range does not
 * reset a pagination offset the user had set deliberately.
 */
export function useRange(): RangeControls {
  const [params, setParams] = useSearchParams()
  const timeZone = useTimeZone()
  const bounds = useListeningBounds()
  const allTimeStart = bounds?.firstListenAt ?? ALL_TIME_START

  const from = params.get('from')
  const to = params.get('to')

  const range = useMemo(
    () => rangeFromParams(new URLSearchParams({ from: from ?? '', to: to ?? '' }), timeZone),
    [from, to, timeZone],
  )

  const write = useCallback(
    (next: DateRange | null) => {
      setParams(
        (current) => {
          const updated = new URLSearchParams(current)
          if (next) {
            updated.set('from', next.from)
            updated.set('to', next.to)
          } else {
            updated.delete('from')
            updated.delete('to')
          }
          // A new range invalidates any page the user had paged to.
          updated.delete('offset')
          updated.delete('cursor')
          return updated
        },
        { replace: true },
      )
    },
    [setParams],
  )

  const setPreset = useCallback(
    (id: RangePresetId) => {
      write(presetRange(id, timeZone, new Date(), allTimeStart))
    },
    [allTimeStart, timeZone, write],
  )

  const setCustomDays = useCallback(
    (fromDay: string, toDay: string) => {
      const start = parseDayInput(fromDay)
      const end = parseDayInput(toDay)
      if (!start || !end) return
      const zone = normaliseZone(timeZone)
      const startAt = startOfDayIn(start, zone)
      // The picker's end day is inclusive; the API's `to` is not.
      const endAt = startOfDayIn(addDays(end, 1), zone)
      if (endAt.getTime() <= startAt.getTime()) return
      write({ from: startAt.toISOString(), to: endAt.toISOString() })
    },
    [timeZone, write],
  )

  const reset = useCallback(() => write(null), [write])

  const zone = normaliseZone(timeZone)
  // Preset matching walks every preset through `Intl`, so it is memoised rather
  // than recomputed on every render of every page that shows the picker.
  const derived = useMemo(
    () => ({
      preset: matchPreset(range, zone),
      label: rangeLabel(range, zone),
      days: {
        from: toDayInput(calendarDayIn(new Date(range.from), zone)),
        to: toDayInput(calendarDayIn(new Date(new Date(range.to).getTime() - 1), zone)),
      },
    }),
    [range, zone],
  )

  return {
    range,
    preset: derived.preset,
    timeZone: zone,
    label: derived.label,
    days: derived.days,
    setPreset,
    setCustomDays,
    reset,
  }
}
