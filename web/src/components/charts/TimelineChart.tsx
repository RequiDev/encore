/**
 * Listening over time.
 *
 * One series, so it is an area rather than a set of coloured lines: the shape is
 * the point, and a single filled band reads it faster than anything else. The
 * fill is a wash at a tenth opacity — the stroke carries the value, the fill
 * only says "this is one thing".
 *
 * Plays and listening time are two different scales, so they are two different
 * renders of the same chart rather than two axes on one plot. A chart with two
 * y-scales invents a correlation the data does not contain.
 */

import type { ReactElement, ReactNode } from 'react'
import { useCallback, useMemo } from 'react'
import { Area, AreaChart, CartesianGrid, Tooltip, XAxis, YAxis } from 'recharts'
import type { TooltipContentProps } from 'recharts'
import { usePrefersReducedMotion } from '../../lib/hooks'
import type { Interval, TimelineBucket } from '../../lib/types'
import {
  formatCompact,
  formatCount,
  formatDate,
  formatDateTime,
  formatDuration,
  formatDurationShort,
  formatMonth,
  formatDayKey,
  formatPlural,
  formatTimeOfDay,
} from '../../lib/format'
import {
  ChartEmpty,
  ChartFrame,
  TOOLTIP_WRAPPER,
  TooltipCard,
  axisLine,
  crosshair,
  numericTick,
} from './ChartFrame'
import { seriesColor, useChartPalette } from './palette'

/** Which figure the timeline plots. Never both at once. */
export type TimelineMetric = 'plays' | 'time'

interface Row {
  key: string
  /** Short form for the axis. */
  label: string
  /** Full form for the tooltip and the summary. */
  full: string
  plays: number
  msPlayed: number
}

/** Exported so charts plotting more than one series over time — genres, say — reuse the same date wording. */
export const INTERVAL_NOUN: Record<Interval, string> = {
  hour: 'hour',
  day: 'day',
  week: 'week',
  month: 'month',
  year: 'year',
}

/**
 * Drops the trailing year from a formatted date, so an axis of days reads
 * "26 Jul" rather than "26 Jul 2026" thirty times over. The full date is still
 * what the tooltip and the summary use.
 */
function withoutYear(value: string): string {
  return value.replace(/\s\d{4}$/, '')
}

/** Exported so a multi-series timeline (the genre chart) shares this exactly rather than re-deriving it. */
export function axisLabelFor(bucket: string, interval: Interval, timeZone: string): string {
  switch (interval) {
    case 'hour':
      return formatTimeOfDay(bucket, timeZone)
    case 'month':
      return formatMonth(bucket, timeZone)
    case 'year':
      // The day key is `2026-07-26`; its first four characters are the year.
      return formatDayKey(bucket, timeZone).slice(0, 4)
    default:
      return withoutYear(formatDate(bucket, timeZone))
  }
}

/** Exported for the same reason as `axisLabelFor`. */
export function fullLabelFor(bucket: string, interval: Interval, timeZone: string): string {
  switch (interval) {
    case 'hour':
      return formatDateTime(bucket, timeZone)
    case 'month':
      return formatMonth(bucket, timeZone)
    case 'year':
      return formatDayKey(bucket, timeZone).slice(0, 4)
    case 'week':
      return `Week of ${formatDate(bucket, timeZone)}`
    default:
      return formatDate(bucket, timeZone)
  }
}

function valueOf(row: Row, metric: TimelineMetric): number {
  return metric === 'plays' ? row.plays : row.msPlayed
}

function formatValue(value: number, metric: TimelineMetric): string {
  return metric === 'plays' ? formatPlural(value, 'play') : formatDuration(value)
}

/** The chart in words, for anyone who cannot see the shape. */
function describe(rows: Row[], metric: TimelineMetric, interval: Interval): string {
  const noun = INTERVAL_NOUN[interval]
  const first = rows[0]
  const last = rows[rows.length - 1]
  if (!first || !last) return 'No listening to plot.'

  const values = rows.map((row) => valueOf(row, metric))
  const total = values.reduce((sum, value) => sum + value, 0)
  let peak = first
  let trough = first
  for (const row of rows) {
    if (valueOf(row, metric) > valueOf(peak, metric)) peak = row
    if (valueOf(row, metric) < valueOf(trough, metric)) trough = row
  }

  const measure = metric === 'plays' ? 'Listens' : 'Listening time'
  const span =
    rows.length === 1 ? first.full : `${first.full} to ${last.full}, one point per ${noun}`
  const totals = `${formatValue(total, metric)} in total.`
  const extremes =
    rows.length === 1
      ? ''
      : ` Busiest ${noun}: ${peak.full}, ${formatValue(valueOf(peak, metric), metric)}.` +
        ` Quietest: ${trough.full}, ${formatValue(valueOf(trough, metric), metric)}.`

  const change = valueOf(last, metric) - valueOf(first, metric)
  const direction =
    rows.length === 1
      ? ''
      : change === 0
        ? ` The last ${noun} matches the first.`
        : ` The last ${noun} is ${formatValue(Math.abs(change), metric)} ${
            change > 0 ? 'above' : 'below'
          } the first.`

  return `${measure} by ${noun}, ${span}. ${totals}${extremes}${direction}`
}

export interface TimelineChartProps {
  buckets: TimelineBucket[]
  /** The bucket size the server chose, which decides how a date is written. */
  interval: Interval
  /** The user's timezone — the one the server bucketed in. */
  timeZone: string
  metric: TimelineMetric
  /** Series slot, for a page that plots this beside something else. */
  slot?: number
  height?: number
  busy?: boolean
  /** Offered when there is nothing to plot. */
  emptyAction?: ReactNode
}

export function TimelineChart({
  buckets,
  interval,
  timeZone,
  metric,
  slot = 0,
  height = 260,
  busy = false,
  emptyAction,
}: TimelineChartProps): ReactElement {
  const palette = useChartPalette()
  const reduced = usePrefersReducedMotion()
  const colour = seriesColor(palette, slot)

  const rows = useMemo<Row[]>(
    () =>
      (buckets ?? []).map((bucket) => ({
        key: bucket.bucket,
        label: axisLabelFor(bucket.bucket, interval, timeZone),
        full: fullLabelFor(bucket.bucket, interval, timeZone),
        plays: bucket.plays,
        msPlayed: bucket.msPlayed,
      })),
    [buckets, interval, timeZone],
  )

  const tooltip = useCallback(
    ({ active, payload }: TooltipContentProps): ReactNode => {
      if (!active) return null
      const row = payload?.[0]?.payload as Row | undefined
      if (!row) return null
      return (
        <TooltipCard
          title={row.full}
          rows={
            metric === 'plays'
              ? [
                  { key: 'plays', color: colour, name: 'plays', value: formatCount(row.plays) },
                  { key: 'time', name: 'listening time', value: formatDuration(row.msPlayed) },
                ]
              : [
                  {
                    key: 'time',
                    color: colour,
                    name: 'listening time',
                    value: formatDuration(row.msPlayed),
                  },
                  { key: 'plays', name: 'plays', value: formatCount(row.plays) },
                ]
          }
        />
      )
    },
    [colour, metric],
  )

  const hasData = rows.some((row) => row.plays > 0 || row.msPlayed > 0)
  if (!hasData) {
    return (
      <ChartEmpty
        height={height}
        description="Nothing was played in this range. Try a wider one, or import more history."
        action={emptyAction}
      />
    )
  }

  const label = metric === 'plays' ? 'Listens over time' : 'Listening time over time'

  return (
    <ChartFrame
      label={label}
      summary={describe(rows, metric, interval)}
      height={height}
      busy={busy}
    >
      {(a11y) => (
        <AreaChart data={rows} margin={{ top: 8, right: 8, bottom: 0, left: 0 }} {...a11y}>
          <CartesianGrid stroke={palette.grid} strokeWidth={1} vertical={false} />
          <XAxis
            // Keyed on the bucket instant, never on the text drawn beneath it.
            // A category axis identifies points by the value of its dataKey, and
            // day and week labels deliberately omit the year — so across a long
            // history the same "26 Jul" recurs once a year, every duplicate
            // collapses onto the first, and hovering anywhere put the marker on
            // the earliest matching bucket. The instant is unique; the label is
            // only ever drawn.
            dataKey="key"
            tickFormatter={(value: string) => axisLabelFor(value, interval, timeZone)}
            tick={numericTick(palette)}
            tickLine={false}
            axisLine={axisLine(palette)}
            minTickGap={28}
            interval="preserveStartEnd"
            height={22}
          />
          <YAxis
            tick={numericTick(palette)}
            tickLine={false}
            axisLine={false}
            width={48}
            allowDecimals={false}
            tickFormatter={(value: number) =>
              metric === 'plays' ? formatCompact(value) : formatDurationShort(value)
            }
          />
          <Tooltip
            content={tooltip}
            cursor={crosshair(palette)}
            wrapperStyle={TOOLTIP_WRAPPER}
            isAnimationActive={!reduced}
          />
          <Area
            dataKey={metric === 'plays' ? 'plays' : 'msPlayed'}
            name={metric === 'plays' ? 'Plays' : 'Listening time'}
            type="linear"
            stroke={colour}
            strokeWidth={2}
            strokeLinecap="round"
            strokeLinejoin="round"
            fill={colour}
            fillOpacity={0.12}
            dot={false}
            activeDot={{ r: 4, strokeWidth: 2, stroke: palette.surface, fill: colour }}
            isAnimationActive={!reduced}
          />
        </AreaChart>
      )}
    </ChartFrame>
  )
}

const METRICS: readonly { id: TimelineMetric; label: string; description: string }[] = [
  { id: 'plays', label: 'Plays', description: 'Show the number of listens' },
  { id: 'time', label: 'Time', description: 'Show listening time' },
]

export interface MetricToggleProps {
  value: TimelineMetric
  onChange: (metric: TimelineMetric) => void
  /** Names the group for assistive technology. */
  label?: string
  className?: string
}

/**
 * Plays or listening time. A radio group rather than a row of buttons: exactly
 * one is chosen at a time, and the arrow keys move between them.
 */
export function MetricToggle({
  value,
  onChange,
  label = 'Chart metric',
  className,
}: MetricToggleProps): ReactElement {
  return (
    <div
      role="radiogroup"
      aria-label={label}
      className={[
        'flex items-center gap-px rounded-control border border-seam-strong p-px',
        className,
      ]
        .filter(Boolean)
        .join(' ')}
    >
      {METRICS.map((metric, index) => (
        <button
          key={metric.id}
          type="button"
          role="radio"
          aria-checked={value === metric.id}
          tabIndex={value === metric.id ? 0 : -1}
          onClick={() => onChange(metric.id)}
          onKeyDown={(event) => {
            if (event.key !== 'ArrowRight' && event.key !== 'ArrowLeft') return
            event.preventDefault()
            const step = event.key === 'ArrowRight' ? 1 : -1
            const at = (index + step + METRICS.length) % METRICS.length
            const next = METRICS[at]
            if (!next) return
            onChange(next.id)
            const sibling = event.currentTarget.parentElement?.children[at]
            if (sibling instanceof HTMLElement) sibling.focus()
          }}
          className={[
            'rounded-chip px-2.5 py-1 text-xs font-medium transition-colors',
            value === metric.id ? 'bg-lamp text-chassis' : 'text-ink-muted hover:text-ink',
          ].join(' ')}
        >
          <span aria-hidden="true">{metric.label}</span>
          <span className="sr-only">{metric.description}</span>
        </button>
      ))}
    </div>
  )
}
