/**
 * The parts every chart in Encore is made of.
 *
 * `ChartFrame` is the important one. A chart is a `<figure>`: the plot itself,
 * and beside it a caption that says in words what the picture says in shape —
 * the total, the extremes, the direction. The caption is visually hidden, so
 * sighted readers see only the chart, but it is a real paragraph in the document
 * rather than an `aria-label`, which means it is reachable, selectable and
 * translatable. Recharts' own accessibility layer stays on, so the plot is a
 * focus stop and the arrow keys walk the series with the tooltip following.
 *
 * Nothing else in the application imports Recharts.
 */

import type { CSSProperties, ReactElement, ReactNode } from 'react'
import { useId } from 'react'
import { ResponsiveContainer } from 'recharts'
import type { TextProps } from 'recharts'
import { EmptyState } from '../ui/EmptyState'
import type { ChartPalette } from './palette'

/** The props a chart element must spread onto its Recharts root. */
export interface ChartA11yProps {
  'aria-label': string
  'aria-describedby': string
}

export interface ChartFrameProps {
  /** Names the plot, e.g. "Listens per day". */
  label: string
  /**
   * The chart in words: what it totals, where it peaks, which way it is going.
   * Written as prose, because it is read as prose.
   */
  summary: string
  /** Total height in pixels, axis band included, so the card never scrolls. */
  height?: number
  /** True while newer data is on its way; the frame dims instead of blanking. */
  busy?: boolean
  className?: string
  children: (a11y: ChartA11yProps) => ReactElement
}

export function ChartFrame({
  label,
  summary,
  height = 240,
  busy = false,
  className,
  children,
}: ChartFrameProps): ReactElement {
  const captionId = useId()

  return (
    <figure className={['m-0', className].filter(Boolean).join(' ')}>
      <div
        // A refetch holds the previous render at reduced opacity rather than
        // dropping back to a skeleton: no layout jump, no flash.
        className={busy ? 'w-full opacity-60 transition-opacity' : 'w-full transition-opacity'}
        style={{ height }}
        aria-busy={busy || undefined}
      >
        {/* The floor keeps the plot legible inside a flex or grid parent that
            would otherwise be happy to squeeze it to nothing. */}
        <ResponsiveContainer width="100%" height="100%" minHeight={Math.min(height, 160)}>
          {children({ 'aria-label': label, 'aria-describedby': captionId })}
        </ResponsiveContainer>
      </div>
      <figcaption id={captionId} className="sr-only">
        {summary}
      </figcaption>
    </figure>
  )
}

export interface ChartEmptyProps {
  /** What is missing, in the reader's terms. */
  title?: string
  description?: ReactNode
  /** The thing that would make this chart non-empty. */
  action?: ReactNode
  height?: number
}

/**
 * A chart with no data draws nothing at all. Empty axes look like a broken
 * request; a sentence saying the range is quiet does not.
 */
export function ChartEmpty({
  title = 'No listens in this range',
  description = 'Widen the date range, or import more of your history.',
  action,
  height = 240,
}: ChartEmptyProps): ReactElement {
  return (
    <div className="flex items-center justify-center" style={{ minHeight: height }}>
      <EmptyState icon="dashboard" title={title} description={description} action={action} />
    </div>
  )
}

export interface TooltipRow {
  key: string
  /** The series colour, drawn as a short stroke beside the figure. */
  color?: string
  /** What the figure is. Set in muted ink — never in the series colour. */
  name: string
  /** Already formatted. */
  value: string
}

/**
 * The hover readout. The value leads and the label follows, which is the
 * legend's hierarchy inverted: here the reader already knows which series they
 * are pointing at and wants the number.
 */
export function TooltipCard({ title, rows }: { title: string; rows: TooltipRow[] }): ReactElement {
  return (
    <div className="panel-raised max-w-56 px-3 py-2">
      <p className="eyebrow">{title}</p>
      <ul className="mt-1.5 space-y-1">
        {rows.map((row) => (
          <li key={row.key} className="flex items-baseline gap-2">
            {row.color ? (
              <span
                aria-hidden="true"
                className="inline-block h-0.5 w-3 shrink-0 self-center rounded-full"
                style={{ backgroundColor: row.color }}
              />
            ) : null}
            <span className="tabular text-sm font-semibold text-ink">{row.value}</span>
            <span className="truncate text-xs text-ink-muted">{row.name}</span>
          </li>
        ))}
      </ul>
    </div>
  )
}

/** Tick text for an axis of figures: the monospace face, so columns line up. */
export function numericTick(palette: ChartPalette): TextProps {
  return { fill: palette.tick, fontSize: 11, fontFamily: palette.mono }
}

/** Tick text for an axis of names. */
export function categoryTick(palette: ChartPalette): TextProps {
  return { fill: palette.tick, fontSize: 11, fontFamily: palette.sans }
}

/** The hairline an axis is ruled with. Solid, one step off the surface. */
export function axisLine(palette: ChartPalette): { stroke: string } {
  return { stroke: palette.grid }
}

/** The crosshair a line or area chart follows the pointer with. */
export function crosshair(palette: ChartPalette): { stroke: string; strokeWidth: number } {
  return { stroke: palette.axis, strokeWidth: 1 }
}

/** The wash a bar chart lifts the hovered band with. */
export function bandCursor(palette: ChartPalette): { fill: string; fillOpacity: number } {
  return { fill: palette.grid, fillOpacity: 0.55 }
}

/** Keeps the tooltip's own focus ring from drawing over the plot. */
export const TOOLTIP_WRAPPER: CSSProperties = { outline: 'none', zIndex: 20 }
