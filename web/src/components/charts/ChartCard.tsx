/**
 * A panel with a chart in it.
 *
 * The only thing this adds to `Panel` is discipline: charts get the same
 * padding, the same eyebrow, and the same place for their one control, so a
 * dashboard of six of them reads as one instrument face rather than six cards
 * that each made their own decisions.
 */

import type { ReactElement, ReactNode } from 'react'
import { Panel } from '../ui/Panel'

export interface ChartCardProps {
  /** The panel's eyebrow: what is plotted. */
  title: string
  /** One line under it, for the units or the caveat. */
  description?: ReactNode
  /**
   * The chart's own control — a metric toggle, an interval switch. Filters that
   * scope the whole page belong above it, not in here.
   */
  control?: ReactNode
  /** Panels are subordinate to the page's single h1. */
  headingLevel?: 2 | 3 | 4
  /** A legend, a scale key, or a sentence under the plot. */
  footer?: ReactNode
  className?: string
  children: ReactNode
}

export function ChartCard({
  title,
  description,
  control,
  headingLevel = 2,
  footer,
  className,
  children,
}: ChartCardProps): ReactElement {
  return (
    <Panel
      title={title}
      description={description}
      actions={control}
      headingLevel={headingLevel}
      padded={false}
      className={className}
      bodyClassName="px-2 pt-4 pb-2 sm:px-3"
    >
      {children}
      {footer ? <div className="mt-2 border-t border-seam px-2 pt-3 pb-1">{footer}</div> : null}
    </Panel>
  )
}
