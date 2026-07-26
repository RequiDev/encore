/**
 * The ledger: Encore's table.
 *
 * Hairline row seams, no zebra striping, numbers right-aligned in the monospace
 * face. The `numeric` prop on a cell sets the `data-numeric` attribute that
 * `index.css` keys the tabular figures off, so a column of counts lines up
 * without anyone remembering to add a class.
 *
 * A ledger always scrolls inside its own container rather than widening the
 * page: on a phone, a nine-column table has to go somewhere.
 */

import type { ReactElement, ReactNode, ThHTMLAttributes, TdHTMLAttributes } from 'react'

export interface LedgerProps {
  /**
   * Describes the table for a screen reader. Required — a table of numbers with
   * no caption is unreadable without sight of the panel it sits in.
   */
  caption: string
  /** Set false to show the caption, which is otherwise for assistive tech only. */
  captionVisible?: boolean
  className?: string
  children: ReactNode
}

export function Ledger({
  caption,
  captionVisible = false,
  className,
  children,
}: LedgerProps): ReactElement {
  return (
    <div className="w-full overflow-x-auto">
      <table className={['ledger', className].filter(Boolean).join(' ')}>
        <caption className={captionVisible ? 'eyebrow px-3 py-2 text-left' : 'sr-only'}>
          {caption}
        </caption>
        {children}
      </table>
    </div>
  )
}

export function LedgerHead({ children }: { children: ReactNode }): ReactElement {
  return <thead>{children}</thead>
}

export function LedgerBody({ children }: { children: ReactNode }): ReactElement {
  return <tbody>{children}</tbody>
}

export interface LedgerRowProps {
  /** Marks the row as the one the user is on, for a detail page's own entry. */
  current?: boolean
  className?: string
  children: ReactNode
}

export function LedgerRow({ current = false, className, children }: LedgerRowProps): ReactElement {
  return (
    <tr aria-current={current ? 'true' : undefined} className={className}>
      {children}
    </tr>
  )
}

export interface LedgerHeaderCellProps extends ThHTMLAttributes<HTMLTableCellElement> {
  /** Right-aligns the column and sets it in tabular figures. */
  numeric?: boolean
}

export function LedgerHeaderCell({
  numeric = false,
  children,
  ...rest
}: LedgerHeaderCellProps): ReactElement {
  return (
    <th scope="col" data-numeric={numeric ? '' : undefined} {...rest}>
      {children}
    </th>
  )
}

export interface LedgerCellProps extends TdHTMLAttributes<HTMLTableCellElement> {
  numeric?: boolean
}

export function LedgerCell({ numeric = false, children, ...rest }: LedgerCellProps): ReactElement {
  return (
    <td data-numeric={numeric ? '' : undefined} {...rest}>
      {children}
    </td>
  )
}

/**
 * The row-header cell that names the thing a row is about. It is a `th` with a
 * row scope, which is what lets a screen reader say "Radiohead, 412 plays"
 * instead of reading a bare number.
 */
export function LedgerRowHeader({
  children,
  ...rest
}: ThHTMLAttributes<HTMLTableCellElement>): ReactElement {
  return (
    <th scope="row" className="font-normal" {...rest}>
      {children}
    </th>
  )
}

export interface LedgerRankProps {
  rank: number
}

/** The rank column: a fixed-width figure so the names beside it start in line. */
export function LedgerRank({ rank }: LedgerRankProps): ReactElement {
  return <span className="tabular text-ink-faint">{rank.toString().padStart(2, '0')}</span>
}
