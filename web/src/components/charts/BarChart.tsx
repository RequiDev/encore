/**
 * Ranked bars, laid on their side.
 *
 * Horizontal because the categories are names — artists, tracks, genres — and a
 * name reads along a row rather than rotated under a column. One series, so one
 * colour for every bar: shading them by value would re-encode what the bar
 * length already says and would spend the only free channel doing it.
 *
 * The value rides the tip of its own bar, so the chart is readable without
 * hovering anything, and the full category name — which the axis may have had
 * to shorten — is in the tooltip and in the written summary.
 */

import type { ReactElement, ReactNode } from 'react'
import { useCallback, useMemo } from 'react'
import {
  Bar,
  BarChart as RechartsBarChart,
  CartesianGrid,
  LabelList,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import type { TooltipContentProps } from 'recharts'
import { usePrefersReducedMotion } from '../../lib/hooks'
import { formatCompact, formatCount, formatRatio } from '../../lib/format'
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
import { seriesColor, useChartPalette } from './palette'

export interface BarDatum {
  /** Stable identity. Colour follows the entity, never its row number. */
  key: string
  label: string
  value: number
  /** A second figure for the tooltip, already formatted. */
  hint?: string
}

export interface BarChartProps {
  data: BarDatum[]
  /** Names the chart: "Top artists by plays". */
  label: string
  /** What one value is, in the tooltip: "plays", "listening time". */
  valueName?: string
  /** How a value is written in the tooltip and at the bar's tip. */
  format?: (value: number) => string
  /** The shorter form the axis uses. */
  axisFormat?: (value: number) => string
  /** Width in pixels reserved for the names. */
  labelWidth?: number
  slot?: number
  /** Total height. Defaults to the row count, so bars keep a constant thickness. */
  height?: number
  busy?: boolean
  emptyAction?: ReactNode
  emptyDescription?: ReactNode
}

const ROW_HEIGHT = 30

export function BarChart({
  data,
  label,
  valueName = 'plays',
  format = formatCount,
  axisFormat = formatCompact,
  labelWidth = 116,
  slot = 0,
  height,
  busy = false,
  emptyAction,
  emptyDescription,
}: BarChartProps): ReactElement {
  const palette = useChartPalette()
  const reduced = usePrefersReducedMotion()
  const colour = seriesColor(palette, slot)

  const rows = useMemo(() => (data ?? []).filter((row) => Number.isFinite(row.value)), [data])
  const total = useMemo(() => rows.reduce((sum, row) => sum + row.value, 0), [rows])

  // Roughly 6.5 pixels a character at 11px; the tooltip and the summary carry
  // the whole name, so shortening here loses nothing.
  const budget = Math.max(8, Math.floor((labelWidth - 12) / 6.5))
  const shorten = useCallback(
    (value: string) => (value.length > budget ? `${value.slice(0, budget - 1)}…` : value),
    [budget],
  )

  const tooltip = useCallback(
    ({ active, payload }: TooltipContentProps): ReactNode => {
      if (!active) return null
      const row = payload?.[0]?.payload as BarDatum | undefined
      if (!row) return null
      return (
        <TooltipCard
          title={row.label}
          rows={[
            { key: 'value', color: colour, name: valueName, value: format(row.value) },
            ...(row.hint ? [{ key: 'hint', name: 'listening time', value: row.hint }] : []),
            { key: 'share', name: 'of the top rows shown', value: formatRatio(row.value, total) },
          ]}
        />
      )
    },
    [colour, format, total, valueName],
  )

  if (rows.length === 0 || total <= 0) {
    return (
      <ChartEmpty
        height={height ?? 200}
        description={
          emptyDescription ?? 'There is nothing to rank in this range yet. Try a wider one.'
        }
        action={emptyAction}
      />
    )
  }

  const leader = rows[0]
  const trailer = rows[rows.length - 1]
  const summary =
    `${label}. ${rows.length === 1 ? 'One row' : `${formatCount(rows.length)} rows`}, ` +
    `led by ${leader?.label ?? ''} with ${format(leader?.value ?? 0)}` +
    (rows.length > 1 && trailer
      ? `, down to ${trailer.label} with ${format(trailer.value)}.`
      : '.') +
    ` The leader is ${formatRatio(leader?.value ?? 0, total)} of the rows shown.`

  return (
    <ChartFrame
      label={label}
      summary={summary}
      height={height ?? rows.length * ROW_HEIGHT + 34}
      busy={busy}
    >
      {(a11y) => (
        <RechartsBarChart
          data={rows}
          layout="vertical"
          margin={{ top: 4, right: 52, bottom: 0, left: 0 }}
          barCategoryGap={2}
          {...a11y}
        >
          <CartesianGrid stroke={palette.grid} strokeWidth={1} horizontal={false} />
          <XAxis
            type="number"
            tick={numericTick(palette)}
            tickLine={false}
            axisLine={axisLine(palette)}
            height={22}
            tickFormatter={axisFormat}
          />
          <YAxis
            type="category"
            dataKey="label"
            tick={categoryTick(palette)}
            tickLine={false}
            axisLine={false}
            width={labelWidth}
            interval={0}
            tickFormatter={shorten}
          />
          <Tooltip
            content={tooltip}
            cursor={bandCursor(palette)}
            wrapperStyle={TOOLTIP_WRAPPER}
            isAnimationActive={!reduced}
          />
          <Bar
            dataKey="value"
            name={valueName}
            fill={colour}
            maxBarSize={22}
            radius={[0, 4, 4, 0]}
            activeBar={{ fillOpacity: 0.75 }}
            isAnimationActive={!reduced}
          >
            <LabelList
              dataKey="value"
              position="right"
              offset={8}
              fill={palette.muted}
              fontSize={11}
              fontFamily={palette.mono}
              formatter={(value) => (typeof value === 'number' ? format(value) : '')}
            />
          </Bar>
        </RechartsBarChart>
      )}
    </ChartFrame>
  )
}
