/**
 * A labelled figure: eyebrow above, counter below.
 *
 * This is the unit the dashboard is built from. It deliberately has no panel of
 * its own — several stats share one panel and are separated by the grid's
 * hairline seams, which reads as a row of gauges on a single face rather than as
 * a scatter of cards.
 */

import type { ReactElement, ReactNode } from 'react'
import { Counter } from './Counter'
import { Skeleton } from './Skeleton'

export interface StatProps {
  label: string
  /** Pre-formatted value. Pass `undefined` while it is still loading. */
  value: ReactNode
  /** A unit set beside the figure: `plays`, `hours`. */
  suffix?: ReactNode
  /** A second line under the figure: a comparison, a date, a caveat. */
  hint?: ReactNode
  /** Share of the whole, 0-1, drawn as a hairline meter. */
  meter?: number
  lamp?: boolean
  loading?: boolean
  className?: string
}

export function Stat({
  label,
  value,
  suffix,
  hint,
  meter,
  lamp = false,
  loading = false,
  className,
}: StatProps): ReactElement {
  return (
    <div className={['min-w-0 p-4', className].filter(Boolean).join(' ')}>
      <p className="eyebrow">{label}</p>
      <div className="mt-2">
        {loading ? (
          <Skeleton className="h-9 w-28" />
        ) : (
          <Counter
            value={value}
            suffix={suffix}
            meter={meter}
            meterLabel={`${label}, share of total`}
            lamp={lamp}
          />
        )}
      </div>
      {hint ? <p className="mt-2 text-xs text-ink-faint">{hint}</p> : null}
    </div>
  )
}

export interface StatGridProps {
  /** Stats per row on a wide screen. Below `sm` it is always one column. */
  columns?: 2 | 3 | 4
  className?: string
  children: ReactNode
}

/**
 * Lays stats out on a single seamed grid. The seams are drawn by the grid's own
 * gap showing the panel behind it, which is why the gap is exactly one pixel.
 */
export function StatGrid({ columns = 4, className, children }: StatGridProps): ReactElement {
  const columnClass =
    columns === 2
      ? 'sm:grid-cols-2'
      : columns === 3
        ? 'sm:grid-cols-2 lg:grid-cols-3'
        : 'sm:grid-cols-2 lg:grid-cols-4'

  return (
    <div
      className={[
        // Each child paints its own background, so the one-pixel gap showing the
        // container through it *is* the seam. No borders to double up at joins.
        'panel grid grid-cols-1 gap-px overflow-hidden bg-seam [&>*]:bg-panel',
        columnClass,
        className,
      ]
        .filter(Boolean)
        .join(' ')}
    >
      {children}
    </div>
  )
}
