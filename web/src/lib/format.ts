/**
 * Every figure Encore shows a person passes through here.
 *
 * Two decisions are worth recording. First, the locale is fixed rather than
 * taken from the browser: a self-hosted instance is often read by several people
 * on several machines, and "26 Jul 2026" means the same thing everywhere while
 * "7/26/2026" and "26/07/2026" do not. Second, dates are always rendered in the
 * user's configured timezone rather than the browser's, because that is the
 * timezone every statistic was bucketed in on the server — showing a listen at
 * a different hour than the histogram that counted it would be a lie.
 */

/** Fixed formatting locale. See the note above; this is deliberate. */
const LOCALE = 'en-GB'

const SECOND = 1000
const MINUTE = 60 * SECOND
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR

function safeMs(ms: number): number {
  if (!Number.isFinite(ms) || ms <= 0) return 0
  return Math.floor(ms)
}

/**
 * Listening time in the coarsest two or three units that still carry meaning:
 * `3d 4h 12m`, `4h 12m`, `12m 30s`, `42s`. Never seconds next to days — nobody
 * reads the seconds on a figure that large.
 */
export function formatDuration(ms: number): string {
  const total = safeMs(ms)
  const days = Math.floor(total / DAY)
  const hours = Math.floor((total % DAY) / HOUR)
  const minutes = Math.floor((total % HOUR) / MINUTE)
  const seconds = Math.floor((total % MINUTE) / SECOND)

  if (days > 0) return `${days}d ${hours}h ${minutes}m`
  if (hours > 0) return `${hours}h ${minutes}m`
  if (minutes > 0) return `${minutes}m ${seconds}s`
  return `${seconds}s`
}

/**
 * The same span with one unit only, for tight places such as a chart axis or a
 * table cell that must not wrap: `3d`, `4h`, `12m`, `42s`.
 */
export function formatDurationShort(ms: number): string {
  const total = safeMs(ms)
  if (total >= DAY) return `${Math.floor(total / DAY)}d`
  if (total >= HOUR) return `${Math.floor(total / HOUR)}h`
  if (total >= MINUTE) return `${Math.floor(total / MINUTE)}m`
  return `${Math.floor(total / SECOND)}s`
}

/**
 * A track length as a clock reading: `3:24`, or `1:02:03` once it passes an
 * hour. This is the form people recognise from a player, so it is used for
 * individual tracks while `formatDuration` is used for totals.
 */
export function formatClock(ms: number): string {
  const total = safeMs(ms)
  const hours = Math.floor(total / HOUR)
  const minutes = Math.floor((total % HOUR) / MINUTE)
  const seconds = Math.floor((total % MINUTE) / SECOND)
  const pad = (n: number) => n.toString().padStart(2, '0')
  if (hours > 0) return `${hours}:${pad(minutes)}:${pad(seconds)}`
  return `${minutes}:${pad(seconds)}`
}

const countFormatter = new Intl.NumberFormat(LOCALE)

/** A count with thousands separators: `1,204,915`. */
export function formatCount(value: number): string {
  if (!Number.isFinite(value)) return '0'
  return countFormatter.format(Math.round(value))
}

const compactFormatter = new Intl.NumberFormat(LOCALE, {
  notation: 'compact',
  maximumFractionDigits: 1,
})

/** A count abbreviated for a narrow column or an axis label: `1.2M`. */
export function formatCompact(value: number): string {
  if (!Number.isFinite(value)) return '0'
  if (Math.abs(value) < 1000) return formatCount(value)
  // ICU changed en-GB's short-thousands suffix from "K" to "k" between
  // versions, so the same number rendered differently depending on which
  // platform ran the code: "1.2K" on this project's Windows machines, "1.2k"
  // in the Linux container Encore actually deploys as. Only the thousands
  // suffix moved — "M" is unchanged — which is why it went unnoticed. Upper
  // case is what the rest of this file documents and what the axis labels
  // were designed against, so normalise rather than let the build host decide
  // what a listener sees. Safe to apply to the whole string: en-GB compact
  // output is digits, a decimal point, an optional minus and one letter.
  return compactFormatter.format(value).toUpperCase()
}

/**
 * A fraction in the range 0-1 as a percentage. Shares are stored as fractions
 * throughout the API, so this is the only conversion point.
 */
export function formatPercent(fraction: number, digits = 1): string {
  if (!Number.isFinite(fraction)) return '0%'
  // Round first, then decide: 0.07 * 100 is 7.000000000000001 in binary floating
  // point, and "7.0%" would be an artefact of that rather than a real precision.
  const rounded = Number((fraction * 100).toFixed(digits))
  // A whole percentage does not need a decimal point; 0.4% very much does.
  return Number.isInteger(rounded) ? `${rounded}%` : `${rounded.toFixed(digits)}%`
}

/** `12 / 48` as a percentage, with a divide-by-zero that reads as zero. */
export function formatRatio(part: number, whole: number, digits = 1): string {
  if (!Number.isFinite(part) || !Number.isFinite(whole) || whole <= 0) return '0%'
  return formatPercent(part / whole, digits)
}

/** A signed change, for deltas against a preceding period: `+3`, `-2`, `0`. */
export function formatSigned(value: number): string {
  if (!Number.isFinite(value)) return '0'
  const rounded = Math.round(value)
  return rounded > 0 ? `+${formatCount(rounded)}` : formatCount(rounded)
}

/** Size of an uploaded export. Binary steps, because that is what a file manager shows. */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit += 1
  }
  const decimals = unit === 0 || value >= 100 ? 0 : value >= 10 ? 1 : 2
  return `${value.toFixed(decimals)} ${units[unit] ?? 'B'}`
}

/** `1 track` / `2 tracks`, with the count formatted. */
export function formatPlural(count: number, singular: string, plural = `${singular}s`): string {
  return `${formatCount(count)} ${Math.abs(Math.round(count)) === 1 ? singular : plural}`
}

// --- rank movement ---------------------------------------------------------

export type RankDirection = 'up' | 'down' | 'flat' | 'new'

export interface RankChange {
  direction: RankDirection
  /** How many places moved. Zero for `flat` and for `new`. */
  places: number
  /** Short label for the cell: `+3`, `-2`, `=`, `NEW`. */
  label: string
  /** Sentence for assistive technology, where `+3` is meaningless. */
  description: string
}

/**
 * Compares a rank with the same entity's rank in the preceding equal-length
 * period. A null `previousRank` means the entity was absent then, which is
 * "new" rather than a rise from infinity.
 */
export function rankChange(rank: number, previousRank: number | null): RankChange {
  if (previousRank === null || !Number.isFinite(previousRank)) {
    return { direction: 'new', places: 0, label: 'NEW', description: 'New this period' }
  }
  const places = previousRank - rank
  if (places === 0) return { direction: 'flat', places: 0, label: '=', description: 'Unchanged' }
  if (places > 0) {
    return {
      direction: 'up',
      places,
      label: `+${places}`,
      description: `Up ${formatPlural(places, 'place')}`,
    }
  }
  return {
    direction: 'down',
    places: -places,
    label: `${places}`,
    description: `Down ${formatPlural(-places, 'place')}`,
  }
}

// --- dates -----------------------------------------------------------------

function toDate(value: Date | string | number): Date | null {
  const date = value instanceof Date ? value : new Date(value)
  return Number.isNaN(date.getTime()) ? null : date
}

const formatterCache = new Map<string, Intl.DateTimeFormat>()

function formatter(timeZone: string, options: Intl.DateTimeFormatOptions): Intl.DateTimeFormat {
  const key = `${timeZone}|${JSON.stringify(options)}`
  const cached = formatterCache.get(key)
  if (cached) return cached
  // Constructing an Intl.DateTimeFormat is expensive enough to matter in a table
  // of a thousand rows, and the set of shapes in use is tiny.
  let made: Intl.DateTimeFormat
  try {
    made = new Intl.DateTimeFormat(LOCALE, { ...options, timeZone })
  } catch {
    made = new Intl.DateTimeFormat(LOCALE, { ...options, timeZone: 'UTC' })
  }
  formatterCache.set(key, made)
  return made
}

/** `26 Jul 2026`. */
export function formatDate(value: Date | string | number, timeZone: string): string {
  const date = toDate(value)
  if (!date) return '—'
  return formatter(timeZone, { day: '2-digit', month: 'short', year: 'numeric' }).format(date)
}

/** `26 Jul 2026, 08:12`. */
export function formatDateTime(value: Date | string | number, timeZone: string): string {
  const date = toDate(value)
  if (!date) return '—'
  return formatter(timeZone, {
    day: '2-digit',
    month: 'short',
    year: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    hourCycle: 'h23',
  }).format(date)
}

/** `08:12`. */
export function formatTimeOfDay(value: Date | string | number, timeZone: string): string {
  const date = toDate(value)
  if (!date) return '—'
  return formatter(timeZone, { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' }).format(date)
}

/** `Jul 2026`, for month buckets on a timeline. */
export function formatMonth(value: Date | string | number, timeZone: string): string {
  const date = toDate(value)
  if (!date) return '—'
  return formatter(timeZone, { month: 'short', year: 'numeric' }).format(date)
}

/** `Mon`, for weekday repartition axes. 0 is Monday, matching the API. */
export function formatWeekday(index: number): string {
  const names = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun']
  return names[((index % 7) + 7) % 7] ?? ''
}

/** `08:00`, for hour-of-day repartition axes. */
export function formatHour(hour: number): string {
  const h = ((Math.trunc(hour) % 24) + 24) % 24
  return `${h.toString().padStart(2, '0')}:00`
}

/**
 * `2026-07-26` in the given timezone. This is the value an `<input type="date">`
 * wants and the key the API uses for a day, so it exists once here rather than
 * being re-derived with `toISOString()` — which would silently use UTC.
 */
export function formatDayKey(value: Date | string | number, timeZone: string): string {
  const date = toDate(value)
  if (!date) return ''
  const parts = formatter(timeZone, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
  }).formatToParts(date)
  const get = (type: Intl.DateTimeFormatPartTypes) =>
    parts.find((p) => p.type === type)?.value ?? ''
  return `${get('year')}-${get('month')}-${get('day')}`
}

const relativeFormatter = new Intl.RelativeTimeFormat(LOCALE, { numeric: 'auto' })

/**
 * `3 minutes ago`, `yesterday`, `in 2 days`. Used for sync times and job
 * timestamps, where the exact instant matters less than the distance from now.
 */
export function formatRelative(value: Date | string | number, now: number = Date.now()): string {
  const date = toDate(value)
  if (!date) return '—'
  const diff = date.getTime() - now
  const abs = Math.abs(diff)

  if (abs < 45 * SECOND) return 'just now'
  const units: [Intl.RelativeTimeFormatUnit, number][] = [
    ['minute', MINUTE],
    ['hour', HOUR],
    ['day', DAY],
    ['week', 7 * DAY],
    ['month', 30 * DAY],
    ['year', 365 * DAY],
  ]
  let chosen: [Intl.RelativeTimeFormatUnit, number] = ['year', 365 * DAY]
  for (let i = 0; i < units.length; i += 1) {
    const unit = units[i]
    const next = units[i + 1]
    if (!unit) continue
    if (!next || abs < next[1]) {
      chosen = unit
      break
    }
  }
  return relativeFormatter.format(Math.round(diff / chosen[1]), chosen[0])
}

/** The dash Encore uses wherever a value is genuinely absent. */
export const EMPTY = '—'
