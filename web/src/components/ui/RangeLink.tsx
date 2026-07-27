/**
 * A link that keeps the selected date range.
 *
 * The range lives in the query string so any view is linkable, which means an
 * ordinary <Link> loses it: pick 2019, click an artist, and the artist page
 * silently falls back to the default thirty days. Every navigation between two
 * range-aware views should go through this instead.
 */

import type { ComponentProps, ReactElement } from 'react'
import { Link, NavLink } from 'react-router-dom'
import { useRangeHref } from '../../lib/range'

export type RangeLinkProps = Omit<ComponentProps<typeof Link>, 'to'> & { to: string }

export function RangeLink({ to, ...rest }: RangeLinkProps): ReactElement {
  const withRange = useRangeHref()
  return <Link to={withRange(to)} {...rest} />
}

export type RangeNavLinkProps = Omit<ComponentProps<typeof NavLink>, 'to'> & { to: string }

/**
 * The navigation equivalent. `isActive` still matches on the path alone, so the
 * appended query string does not disturb which item is highlighted.
 */
export function RangeNavLink({ to, ...rest }: RangeNavLinkProps): ReactElement {
  const withRange = useRangeHref()
  return <NavLink to={withRange(to)} {...rest} />
}
