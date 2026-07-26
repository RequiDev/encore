/**
 * Pagination, in the two shapes the API offers.
 *
 * `Pagination` is for the offset-paginated lists. `CursorPagination` is for the
 * listening history, which is keyset paginated because a user may legitimately
 * have millions of rows — it can go forward and back through pages it has
 * already seen, but it cannot jump, and it does not pretend to know how many
 * pages there are.
 */

import type { ReactElement } from 'react'
import { formatCount } from '../../lib/format'
import { Button } from './Button'
import { Icon } from './Icon'

export interface PaginationProps {
  /** Total rows the server reports for the query. */
  total: number
  limit: number
  offset: number
  /** Called with the new offset. */
  onChange: (offset: number) => void
  /** Names the list, so the controls are distinguishable when there are two. */
  label?: string
  disabled?: boolean
  className?: string
}

export function Pagination({
  total,
  limit,
  offset,
  onChange,
  label = 'Results',
  disabled = false,
  className,
}: PaginationProps): ReactElement | null {
  if (total <= limit) return null

  const page = Math.floor(offset / limit) + 1
  const pages = Math.max(Math.ceil(total / limit), 1)
  const first = total === 0 ? 0 : offset + 1
  const last = Math.min(offset + limit, total)

  return (
    <nav
      aria-label={`${label} pagination`}
      className={['flex items-center justify-between gap-3 px-4 py-3', className]
        .filter(Boolean)
        .join(' ')}
    >
      <p className="text-xs text-ink-muted">
        <span className="tabular">{formatCount(first)}</span>–
        <span className="tabular">{formatCount(last)}</span> of{' '}
        <span className="tabular">{formatCount(total)}</span>
      </p>
      <div className="flex items-center gap-2">
        <Button
          size="sm"
          disabled={disabled || offset <= 0}
          onClick={() => onChange(Math.max(offset - limit, 0))}
        >
          <Icon name="chevron-left" />
          Previous
        </Button>
        <p className="eyebrow px-1">
          <span className="tabular">{page}</span> / <span className="tabular">{pages}</span>
        </p>
        <Button
          size="sm"
          disabled={disabled || last >= total}
          onClick={() => onChange(offset + limit)}
        >
          Next
          <Icon name="chevron-right" />
        </Button>
      </div>
    </nav>
  )
}

export interface CursorPaginationProps {
  /** The server's own answer; never inferred from a short page. */
  hasMore: boolean
  /** False on the first page, where there is nothing to go back to. */
  hasPrevious: boolean
  onNext: () => void
  onPrevious: () => void
  /** How many rows the current page holds, for the count beside the controls. */
  count?: number
  label?: string
  loading?: boolean
  className?: string
}

export function CursorPagination({
  hasMore,
  hasPrevious,
  onNext,
  onPrevious,
  count,
  label = 'History',
  loading = false,
  className,
}: CursorPaginationProps): ReactElement {
  return (
    <nav
      aria-label={`${label} pagination`}
      className={['flex items-center justify-between gap-3 px-4 py-3', className]
        .filter(Boolean)
        .join(' ')}
    >
      <p className="text-xs text-ink-muted">
        {count === undefined ? null : (
          <>
            <span className="tabular">{formatCount(count)}</span> on this page
          </>
        )}
      </p>
      <div className="flex items-center gap-2">
        <Button size="sm" disabled={!hasPrevious || loading} onClick={onPrevious}>
          <Icon name="chevron-left" />
          Newer
        </Button>
        <Button size="sm" disabled={!hasMore} busy={loading} onClick={onNext}>
          Older
          <Icon name="chevron-right" />
        </Button>
      </div>
    </nav>
  )
}
