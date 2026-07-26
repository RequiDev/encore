/**
 * The top of a page: its one h1, and the controls that scope it.
 *
 * Every route renders exactly one of these. That is how the "one h1 per page"
 * rule stays true as more pages are added — the heading is not something a page
 * author has to remember to mark up correctly, it is something they get by
 * using the component.
 */

import type { ReactElement, ReactNode } from 'react'
import { useDocumentTitle } from '../../lib/hooks'

export interface PageHeaderProps {
  title: string
  /** One line under the title: what this page counts, and over what. */
  description?: ReactNode
  /** The range picker, a search box, an action — aligned right on wide screens. */
  actions?: ReactNode
  /**
   * Overrides the browser tab title. Defaults to `title`, which is right almost
   * always; a detail page passes the entity's name.
   */
  documentTitle?: string
  className?: string
}

export function PageHeader({
  title,
  description,
  actions,
  documentTitle,
  className,
}: PageHeaderProps): ReactElement {
  useDocumentTitle(documentTitle ?? title)

  return (
    <header
      className={[
        'flex flex-col gap-3 border-b border-seam pb-4 md:flex-row md:items-end md:justify-between',
        className,
      ]
        .filter(Boolean)
        .join(' ')}
    >
      <div className="min-w-0">
        <h1 className="truncate text-xl font-semibold tracking-tight text-ink">{title}</h1>
        {description ? <p className="mt-1 text-sm text-ink-muted">{description}</p> : null}
      </div>
      {actions ? <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div> : null}
    </header>
  )
}
