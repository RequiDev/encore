/**
 * Loading placeholders.
 *
 * They mirror the shape of what is coming so the page does not jump when the
 * data lands. The pulse is a CSS animation, which the reduced-motion rule in
 * `index.css` already flattens to a static block for anyone who asked for that.
 */

import type { ReactElement } from 'react'

export interface SkeletonProps {
  /** Size and shape come from utilities: `h-9 w-28`, `h-4 w-full`. */
  className?: string
}

export function Skeleton({ className }: SkeletonProps): ReactElement {
  return (
    <span
      aria-hidden="true"
      className={['block animate-pulse rounded-control bg-seam', className]
        .filter(Boolean)
        .join(' ')}
    />
  )
}

export interface SkeletonTextProps {
  lines?: number
  className?: string
}

/** A paragraph's worth of placeholder, with a short last line. */
export function SkeletonText({ lines = 3, className }: SkeletonTextProps): ReactElement {
  return (
    <div className={['space-y-2', className].filter(Boolean).join(' ')}>
      {Array.from({ length: lines }, (_, i) => (
        <Skeleton key={i} className={i === lines - 1 ? 'h-3 w-2/5' : 'h-3 w-full'} />
      ))}
    </div>
  )
}

export interface SkeletonLedgerProps {
  rows?: number
  columns?: number
  className?: string
}

/**
 * A ledger-shaped placeholder. The first column is wide and the rest are narrow,
 * which is the shape of every table in Encore: a name, then figures.
 */
export function SkeletonLedger({
  rows = 8,
  columns = 3,
  className,
}: SkeletonLedgerProps): ReactElement {
  return (
    <div
      className={['divide-y divide-seam', className].filter(Boolean).join(' ')}
      role="status"
      aria-live="polite"
      aria-busy="true"
      aria-label="Loading rows"
    >
      {Array.from({ length: rows }, (_, row) => (
        <div key={row} className="flex items-center gap-3 px-3 py-2.5">
          <Skeleton className="h-3 flex-1" />
          {Array.from({ length: Math.max(columns - 1, 0) }, (_, col) => (
            <Skeleton key={col} className="h-3 w-14" />
          ))}
        </div>
      ))}
    </div>
  )
}
