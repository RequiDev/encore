/**
 * The column chart behind the hour-of-day and day-of-week repartitions.
 *
 * Both answer the same question — how much listening falls in each slot of a
 * cycle — so they are one chart with two sets of labels rather than two charts
 * that drift apart. The categories are ordered by the clock, and the axis
 * already carries that order, so every column takes the same colour: shading
 * them by value would spend the identity channel on what the bar height says.
 *
 * Not exported from the kit's index; `HourChart` and `WeekdayChart` are.
 */

import type { ReactElement, ReactNode } from 'react'
import { useCallback, useMemo } from 'react'
import { Bar, BarChart, CartesianGrid, Tooltip, XAxis, YAxis } from 'recharts'
import type { TooltipContentProps } from 'recharts'
import { usePrefersReducedMotion } from '../../lib/hooks'
import type { RepartitionBucket } from '../../lib/types'
import {
  formatCompact,
  formatCount,
  formatDuration,
  formatDurationShort,
  formatPlural,
  formatRatio,
} from '../../lib/format'
import {
  ChartEmpty,
  ChartFrame,
  TOOLTIP_WRAPPER,
  TooltipCard,
  axisLine,
  bandCursor,
  categoryTick,
  numericTick,
} from './ChartFrame'
import type { TimelineMetric } from './TimelineChart'
import { seriesColor, useChartPalette } from './palette'

interface Row {
  key: number
  label: string
  full: string
  plays: number
  msPlayed: number
}

function valueOf(row: Row, metric: TimelineMetric): number {
  return metric === 'plays' ? row.plays : row.msPlayed
}

function formatValue(value: number, metric: TimelineMetric): string {
  return metric === 'plays' ? formatPlural(value, 'play') : formatDuration(value)
}

function describe(rows: Row[], metric: TimelineMetric, label: string, noun: string): string {
  if (rows.length === 0) return 'Nothing to plot.'
  const total = rows.reduce((sum, row) => sum + valueOf(row, metric), 0)
  const ranked = [...rows].sort((a, b) => valueOf(b, metric) - valueOf(a, metric))
  const busiest = ranked[0]
  const quietest = ranked[ranked.length - 1]
  if (!busiest || !quietest) return 'Nothing to plot.'

  const runners = ranked
    .slice(1, 3)
    .map((row) => `${row.full}, ${formatValue(valueOf(row, metric), metric)}`)
    .join(' and ')

  return (
    `${label}. ${formatValue(total, metric)} in total.` +
    ` Busiest ${noun}: ${busiest.full}, ${formatValue(valueOf(busiest, metric), metric)}` +
    ` — ${formatRatio(valueOf(busiest, metric), total)} of the range.` +
    (runners ? ` Then ${runners}.` : '') +
    ` Quietest: ${quietest.full}, ${formatValue(valueOf(quietest, metric), metric)}.`
  )
}

export interface RepartitionColumnsProps {
  buckets: RepartitionBucket[]
  metric: TimelineMetric
  /** Short label for the axis. Return an empty string to leave a tick blank. */
  axisLabel: (key: number) => string
  /** Full name, used by the tooltip and the written summary. */
  fullLabel: (key: number) => string
  /** Names the chart: "Listens by hour of day". */
  label: string
  /** What one column is: "hour", "day". */
  noun: string
  slot?: number
  height?: number
  busy?: boolean
  emptyAction?: ReactNode
}

export function RepartitionColumns({
  buckets,
  metric,
  axisLabel,
  fullLabel,
  label,
  noun,
  slot = 0,
  height = 220,
  busy = false,
  emptyAction,
}: RepartitionColumnsProps): ReactElement {
  const palette = useChartPalette()
  const reduced = usePrefersReducedMotion()
  const colour = seriesColor(palette, slot)

  const rows = useMemo<Row[]>(
    () =>
      (buckets ?? []).map((bucket) => ({
        key: bucket.key,
        label: axisLabel(bucket.key),
        full: fullLabel(bucket.key),
        plays: bucket.plays,
        msPlayed: bucket.msPlayed,
      })),
    [buckets, axisLabel, fullLabel],
  )

  const total = useMemo(
    () => rows.reduce((sum, row) => sum + valueOf(row, metric), 0),
    [rows, metric],
  )

  const tooltip = useCallback(
    ({ active, payload }: TooltipContentProps): ReactNode => {
      if (!active) return null
      const row = payload?.[0]?.payload as Row | undefined
      if (!row) return null
      const value = valueOf(row, metric)
      return (
        <TooltipCard
          title={row.full}
          rows={[
            {
              key: 'value',
              color: colour,
              name: metric === 'plays' ? 'plays' : 'listening time',
              value: metric === 'plays' ? formatCount(row.plays) : formatDuration(row.msPlayed),
            },
            { key: 'share', name: 'of the range', value: formatRatio(value, total) },
          ]}
        />
      )
    },
    [colour, metric, total],
  )

  if (total <= 0) {
    return (
      <ChartEmpty
        height={height}
        description="Nothing was played in this range, so there is no pattern to show yet."
        action={emptyAction}
      />
    )
  }

  return (
    <ChartFrame
      label={label}
      summary={describe(rows, metric, label, noun)}
      height={height}
      busy={busy}
    >
      {(a11y) => (
        <BarChart
          data={rows}
          margin={{ top: 8, right: 8, bottom: 0, left: 0 }}
          barCategoryGap={2}
          {...a11y}
        >
          <CartesianGrid stroke={palette.grid} strokeWidth={1} vertical={false} />
          <XAxis
            dataKey="label"
            tick={categoryTick(palette)}
            tickLine={false}
            axisLine={axisLine(palette)}
            interval={0}
            height={22}
          />
          <YAxis
            tick={numericTick(palette)}
            tickLine={false}
            axisLine={false}
            width={44}
            allowDecimals={false}
            tickFormatter={(value: number) =>
              metric === 'plays' ? formatCompact(value) : formatDurationShort(value)
            }
          />
          <Tooltip
            content={tooltip}
            cursor={bandCursor(palette)}
            wrapperStyle={TOOLTIP_WRAPPER}
            isAnimationActive={!reduced}
          />
          <Bar
            dataKey={metric === 'plays' ? 'plays' : 'msPlayed'}
            name={metric === 'plays' ? 'Plays' : 'Listening time'}
            fill={colour}
            maxBarSize={24}
            radius={[4, 4, 0, 0]}
            activeBar={{ fillOpacity: 0.75 }}
            isAnimationActive={!reduced}
          />
        </BarChart>
      )}
    </ChartFrame>
  )
}
