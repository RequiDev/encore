/**
 * A trend the size of a word.
 *
 * Hand-drawn SVG rather than Recharts: at ninety pixels wide there are no axes,
 * no grid, no tooltip and no responsive container to justify the machinery, and
 * a stat tile should not pay for a chart engine to draw twelve line segments.
 *
 * It carries no numbers of its own. The tile beside it already says the total
 * and the change; the sparkline only says which way the line went, which is why
 * its accessible name is a sentence rather than a list of values.
 */

import type { ReactElement } from 'react'
import { useId } from 'react'
import { formatCount, formatPlural } from '../../lib/format'
import { seriesColor, useChartPalette } from './palette'

export interface SparklineProps {
  /** One value per bucket, oldest first. */
  values: number[]
  /** What the values are, for the accessible name: "listens per day". */
  label: string
  /** How one value is written in that name. */
  format?: (value: number) => string
  width?: number
  height?: number
  slot?: number
  className?: string
}

export function Sparkline({
  values,
  label,
  format = formatCount,
  width = 96,
  height = 24,
  slot = 0,
  className,
}: SparklineProps): ReactElement | null {
  const palette = useChartPalette()
  const titleId = useId()
  const colour = seriesColor(palette, slot)

  const points = (values ?? []).filter((value) => Number.isFinite(value))
  // One point is not a trend, and a flat pair of zeroes is not worth the ink.
  if (points.length < 2) return null

  const max = Math.max(...points)
  const min = Math.min(...points)
  const span = max - min || 1
  const step = points.length > 1 ? width / (points.length - 1) : width
  // Half the stroke at each edge, so a peak is not clipped by the viewBox.
  const top = 1.5
  const usable = height - 3

  const coords = points.map((value, index) => {
    const x = index * step
    const y = top + usable - ((value - min) / span) * usable
    return [x, y] as const
  })
  const last = coords[coords.length - 1]
  const path = coords.map(([x, y], i) => `${i === 0 ? 'M' : 'L'}${x.toFixed(1)} ${y.toFixed(1)}`)

  const first = points[0] ?? 0
  const latest = points[points.length - 1] ?? 0
  const direction = latest === first ? 'level with' : latest > first ? 'up from' : 'down from'
  const summary =
    `${label}: ${formatPlural(points.length, 'point')}, ${direction} ${format(first)} to ${format(latest)}.` +
    ` Highest ${format(max)}, lowest ${format(min)}.`

  return (
    <svg
      role="img"
      aria-labelledby={titleId}
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      className={className}
      focusable="false"
    >
      <title id={titleId}>{summary}</title>
      <path
        d={path.join(' ')}
        fill="none"
        stroke={colour}
        strokeWidth={1.5}
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      {last ? (
        // A surface ring keeps the end dot legible where it crosses the line.
        <circle
          cx={last[0]}
          cy={last[1]}
          r={2}
          fill={colour}
          stroke={palette.surface}
          strokeWidth={1.5}
        />
      ) : null}
    </svg>
  )
}
