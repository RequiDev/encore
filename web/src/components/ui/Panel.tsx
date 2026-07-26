/**
 * A panel: one face on the chassis.
 *
 * Flat, hairline-seamed, no shadow — panels sit *in* the surface rather than
 * floating above it. The header is an eyebrow rather than a heading-sized title
 * because a dense statistics page has a dozen of them and full-size headings
 * would compete with the page's own h1.
 */

import type { ElementType, ReactElement, ReactNode } from 'react'

export interface PanelProps {
  /** Panel label, rendered as an eyebrow. */
  title?: ReactNode
  /** One line under the title, for the units or the caveat. */
  description?: ReactNode
  /** Controls aligned to the right of the header. */
  actions?: ReactNode
  /**
   * Heading level for the title. Panels are subordinate to the page's single
   * h1, so h2 is the default and h3 is right for a panel nested inside one.
   */
  headingLevel?: 2 | 3 | 4
  /** Set false when the child draws to the panel's own edges, such as a table. */
  padded?: boolean
  /** Raises the panel one step for content that sits on top of other content. */
  raised?: boolean
  className?: string
  bodyClassName?: string
  children?: ReactNode
}

export function Panel({
  title,
  description,
  actions,
  headingLevel = 2,
  padded = true,
  raised = false,
  className,
  bodyClassName,
  children,
}: PanelProps): ReactElement {
  const Heading = `h${headingLevel}` as ElementType
  const hasHeader = Boolean(title || actions || description)

  return (
    <section
      className={[raised ? 'panel-raised' : 'panel', 'min-w-0', className]
        .filter(Boolean)
        .join(' ')}
    >
      {hasHeader ? (
        <header className="flex items-start justify-between gap-3 border-b border-seam px-4 py-2.5">
          <div className="min-w-0">
            {title ? <Heading className="eyebrow truncate">{title}</Heading> : null}
            {description ? <p className="mt-1 text-xs text-ink-faint">{description}</p> : null}
          </div>
          {actions ? <div className="flex shrink-0 items-center gap-2">{actions}</div> : null}
        </header>
      ) : null}
      <div className={[padded ? 'p-4' : '', bodyClassName].filter(Boolean).join(' ')}>
        {children}
      </div>
    </section>
  )
}

/**
 * A hairline divider inside a panel, for separating a summary from its detail
 * without starting a second panel.
 */
export function PanelDivider(): ReactElement {
  return <hr className="border-0 border-t border-seam" />
}
