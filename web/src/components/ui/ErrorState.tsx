/**
 * A request failed, said plainly.
 *
 * The server's own message is shown, because `docs/api.md` guarantees it is
 * written for a person and contains no tokens, credentials or SQL. The machine
 * code goes underneath in small type: it is what someone would quote in a bug
 * report, and hiding it helps nobody.
 */

import type { ReactElement, ReactNode } from 'react'
import { ApiError } from '../../lib/api'
import { Button } from './Button'
import { Icon } from './Icon'

export interface ErrorStateProps {
  /** Whatever the query threw. Anything non-`ApiError` gets a generic message. */
  error: unknown
  /** Overrides the heading, for cases where the caller has more context. */
  title?: string
  /** Shown as a button when the failure is worth another attempt. */
  onRetry?: () => void
  className?: string
  children?: ReactNode
}

/** A message safe to put in front of a person, whatever was thrown. */
export function errorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message
  if (error instanceof Error && error.message) return error.message
  return 'Something went wrong.'
}

function errorCode(error: unknown): string | null {
  return error instanceof ApiError ? error.code : null
}

export function ErrorState({
  error,
  title,
  onRetry,
  className,
  children,
}: ErrorStateProps): ReactElement {
  const code = errorCode(error)
  const heading =
    title ?? (error instanceof ApiError && error.isNotFound ? 'Not found' : 'That did not work')

  return (
    <div
      // Assertive rather than polite: this replaced content the person was
      // waiting for, so it should interrupt rather than queue.
      role="alert"
      className={['flex flex-col items-center px-6 py-12 text-center', className]
        .filter(Boolean)
        .join(' ')}
    >
      <span className="mb-3 text-ember">
        <Icon name="warning" size={28} />
      </span>
      <p className="text-sm font-medium text-ink">{heading}</p>
      <p className="mt-1.5 max-w-prose text-sm text-ink-muted">{errorMessage(error)}</p>
      {code ? <p className="tabular mt-2 text-xs text-ink-faint">{code}</p> : null}
      {children ? <div className="mt-4">{children}</div> : null}
      {onRetry ? (
        <div className="mt-4">
          <Button onClick={onRetry}>
            <Icon name="refresh" />
            Try again
          </Button>
        </div>
      ) : null}
    </div>
  )
}
