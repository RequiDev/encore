/**
 * The navigation map.
 *
 * One list, read by the sidebar, the mobile bar and the drawer, so a route added
 * here appears everywhere it should and nowhere twice.
 */

import type { IconName } from '../ui/Icon'

export interface NavItem {
  to: string
  label: string
  icon: IconName
  /**
   * Match only the exact path. Needed for `/` which would otherwise be the
   * active parent of every route in the application.
   */
  exact?: boolean
  /** Shown in the phone bottom bar, which has room for four destinations. */
  primary?: boolean
  /**
   * The destination reads the date range, so navigating to it should carry the
   * one already chosen rather than dropping the viewer back to the default.
   * False for pages that have no notion of a range — appending one there would
   * be noise in the address bar and nothing else.
   */
  ranged?: boolean
}

export interface NavSection {
  /** Section legend. Empty for the first group, which needs no heading. */
  title: string
  items: NavItem[]
}

export const NAV_SECTIONS: readonly NavSection[] = [
  {
    title: 'Listening',
    items: [
      { ranged: true, to: '/', label: 'Dashboard', icon: 'dashboard', exact: true, primary: true },
      { ranged: true, to: '/history', label: 'History', icon: 'history', primary: true },
      { ranged: true, to: '/search', label: 'Search', icon: 'search' },
    ],
  },
  {
    title: 'Rankings',
    items: [
      { ranged: true, to: '/artists', label: 'Artists', icon: 'artist', primary: true },
      { ranged: true, to: '/albums', label: 'Albums', icon: 'album' },
      { ranged: true, to: '/tracks', label: 'Tracks', icon: 'track' },
    ],
  },
  {
    title: 'Patterns',
    items: [
      { ranged: true, to: '/sessions', label: 'Sessions', icon: 'session' },
      { ranged: true, to: '/discovery', label: 'Discovery', icon: 'discovery' },
      { ranged: true, to: '/streaks', label: 'Streaks', icon: 'streak' },
    ],
  },
  {
    title: 'Retrospect',
    items: [
      { to: '/year', label: 'Year in review', icon: 'year' },
      { to: '/compare', label: 'Compare', icon: 'compare' },
    ],
  },
  {
    title: 'Data',
    items: [
      { to: '/imports', label: 'Imports', icon: 'import' },
      { to: '/settings', label: 'Settings', icon: 'settings' },
    ],
  },
]

/** Flattened, in sidebar order. */
export const NAV_ITEMS: readonly NavItem[] = NAV_SECTIONS.flatMap((section) => section.items)

/** The destinations the phone bottom bar shows beside its "More" button. */
export const PRIMARY_NAV: readonly NavItem[] = NAV_ITEMS.filter((item) => item.primary)

/**
 * The heading a route should use in the page-change announcement, falling back
 * to the path so an unmapped route still announces something truthful.
 */
export function navTitleFor(pathname: string): string {
  const exact = NAV_ITEMS.find((item) => item.to === pathname)
  if (exact) return exact.label
  const nested = NAV_ITEMS.filter((item) => item.to !== '/').find((item) =>
    pathname.startsWith(`${item.to}/`),
  )
  return nested?.label ?? 'Encore'
}
