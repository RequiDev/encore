/**
 * Nothing to show, and why.
 *
 * An empty statistics page is usually not a bug — it is a range with no listens
 * in it, or an instance that has not imported anything yet — so the copy names
 * the likely cause and offers the action that fixes it.
 */

import type { ReactElement, ReactNode } from 'react'
import { Icon } from './Icon'
import type { IconName } from './Icon'

export interface EmptyStateProps {
  title: string
  description?: ReactNode
  /** The mark above the title. Defaults to the equaliser. */
  icon?: IconName
  /** One button, usually the thing that would make this page non-empty. */
  action?: ReactNode
  className?: string
}

export function EmptyState({
  title,
  description,
  icon = 'dashboard',
  action,
  className,
}: EmptyStateProps): ReactElement {
  return (
    <div
      className={['flex flex-col items-center px-6 py-12 text-center', className]
        .filter(Boolean)
        .join(' ')}
    >
      <span className="mb-3 text-ink-faint">
        <Icon name={icon} size={28} />
      </span>
      <p className="text-sm font-medium text-ink">{title}</p>
      {description ? (
        <p className="mt-1.5 max-w-prose text-sm text-ink-muted">{description}</p>
      ) : null}
      {action ? <div className="mt-4">{action}</div> : null}
    </div>
  )
}
