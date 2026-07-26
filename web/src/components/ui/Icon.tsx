/**
 * The icon set.
 *
 * Encore ships no icon library. The whole application needs about twenty marks,
 * they are all simple geometry, and an inline set costs nothing at runtime while
 * a dependency would cost a download, a licence and an upgrade path. Everything
 * is drawn as a 24-unit stroke path in `currentColor`, so an icon inherits the
 * colour and the theme of whatever it sits in.
 */

import type { ReactElement, ReactNode } from 'react'

export type IconName =
  | 'dashboard'
  | 'history'
  | 'search'
  | 'artist'
  | 'album'
  | 'track'
  | 'session'
  | 'discovery'
  | 'streak'
  | 'year'
  | 'compare'
  | 'import'
  | 'settings'
  | 'admin'
  | 'sun'
  | 'moon'
  | 'system'
  | 'chevron-left'
  | 'chevron-right'
  | 'chevron-down'
  | 'close'
  | 'menu'
  | 'more'
  | 'external'
  | 'warning'
  | 'check'
  | 'info'
  | 'refresh'
  | 'logout'
  | 'calendar'

const PATHS: Record<IconName, ReactNode> = {
  // A three-band equaliser: the dashboard is levels at a glance.
  dashboard: <path d="M3 21h18M7 21V11M12 21V4M17 21V15" />,
  history: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="M12 7v5.5l3.5 2" />
    </>
  ),
  search: (
    <>
      <circle cx="11" cy="11" r="7" />
      <path d="M16.5 16.5 21 21" />
    </>
  ),
  artist: (
    <>
      <circle cx="12" cy="8" r="4" />
      <path d="M4.5 20.5c0-3.6 3.4-5.5 7.5-5.5s7.5 1.9 7.5 5.5" />
    </>
  ),
  album: (
    <>
      <circle cx="12" cy="12" r="9" />
      <circle cx="12" cy="12" r="2.25" />
    </>
  ),
  track: (
    <>
      <path d="M9 17V5l11-2v12" />
      <circle cx="6" cy="17" r="3" />
      <circle cx="17" cy="15" r="3" />
    </>
  ),
  session: <path d="M2 12h3.5l2.5-7 3.5 14 3-9 2 4h5.5" />,
  discovery: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="m15.5 8.5-2.2 4.8-4.8 2.2 2.2-4.8z" />
    </>
  ),
  streak: (
    <path d="M12 3c3.2 3.9 5.5 6.2 5.5 9.2a5.5 5.5 0 1 1-11 0c0-1.7.8-3 1.9-4 .1 1.6.9 2.3 1.6 1.9C11.7 9.2 11 5.6 12 3z" />
  ),
  year: (
    <>
      <rect x="3" y="5" width="18" height="16" rx="1.5" />
      <path d="M8 3v4M16 3v4M3 10h18" />
    </>
  ),
  compare: <path d="M3 9h13m-3-3 3 3-3 3M21 15H8m3-3-3 3 3 3" />,
  import: (
    <>
      <path d="M12 15V3m-4 4 4-4 4 4" />
      <path d="M4 15v4.5a1.5 1.5 0 0 0 1.5 1.5h13a1.5 1.5 0 0 0 1.5-1.5V15" />
    </>
  ),
  settings: (
    <>
      <path d="M4 7h16M4 12h16M4 17h16" />
      <circle cx="9" cy="7" r="1.75" />
      <circle cx="15" cy="12" r="1.75" />
      <circle cx="7" cy="17" r="1.75" />
    </>
  ),
  admin: (
    <>
      <path d="M12 3.5 20 6.5v5.4c0 4.7-3.3 7.8-8 9.1-4.7-1.3-8-4.4-8-9.1V6.5z" />
      <path d="m9 12 2.2 2.2L15.5 10" />
    </>
  ),
  sun: (
    <>
      <circle cx="12" cy="12" r="4" />
      <path d="M12 2v2.5M12 19.5V22M2 12h2.5M19.5 12H22M4.9 4.9l1.8 1.8M17.3 17.3l1.8 1.8M19.1 4.9l-1.8 1.8M6.7 17.3l-1.8 1.8" />
    </>
  ),
  moon: <path d="M20.5 14.3A8.6 8.6 0 0 1 9.7 3.5a8.6 8.6 0 1 0 10.8 10.8z" />,
  system: (
    <>
      <rect x="3" y="4" width="18" height="12.5" rx="1.5" />
      <path d="M9 20.5h6M12 16.5v4" />
    </>
  ),
  'chevron-left': <path d="m14.5 6-6 6 6 6" />,
  'chevron-right': <path d="m9.5 6 6 6-6 6" />,
  'chevron-down': <path d="m6 9.5 6 6 6-6" />,
  close: <path d="m6 6 12 12M18 6 6 18" />,
  menu: <path d="M4 7h16M4 12h16M4 17h16" />,
  more: (
    <>
      <circle cx="5.5" cy="12" r="1.25" />
      <circle cx="12" cy="12" r="1.25" />
      <circle cx="18.5" cy="12" r="1.25" />
    </>
  ),
  external: (
    <>
      <path d="M14 4h6v6M20 4l-8.5 8.5" />
      <path d="M18 14.5V19a1.5 1.5 0 0 1-1.5 1.5h-11A1.5 1.5 0 0 1 4 19V8a1.5 1.5 0 0 1 1.5-1.5H10" />
    </>
  ),
  warning: (
    <>
      <path d="M12 3.8 21.2 20H2.8z" />
      <path d="M12 10v4.5M12 17.5h.01" />
    </>
  ),
  check: <path d="m5 12.5 4.5 4.5L19 7" />,
  info: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="M12 11v5.5M12 7.75h.01" />
    </>
  ),
  refresh: (
    <>
      <path d="M20 12a8 8 0 1 1-2.6-5.9" />
      <path d="M20.5 4v5h-5" />
    </>
  ),
  logout: (
    <>
      <path d="M15 12H4m4-4-4 4 4 4" />
      <path d="M12 4.5h6.5A1.5 1.5 0 0 1 20 6v12a1.5 1.5 0 0 1-1.5 1.5H12" />
    </>
  ),
  calendar: (
    <>
      <rect x="3" y="5" width="18" height="16" rx="1.5" />
      <path d="M8 3v4M16 3v4M3 10h18" />
    </>
  ),
}

export interface IconProps {
  name: IconName
  /** Edge length in pixels. Icons are drawn on a 24-unit grid and scale cleanly. */
  size?: number
  className?: string
  /**
   * An accessible name. Omit it — the usual case — and the icon is hidden from
   * assistive technology, because it sits beside a label that already says this.
   */
  title?: string
}

export function Icon({ name, size = 16, className, title }: IconProps): ReactElement {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.6}
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      aria-hidden={title ? undefined : true}
      role={title ? 'img' : undefined}
      focusable="false"
    >
      {title ? <title>{title}</title> : null}
      {PATHS[name]}
    </svg>
  )
}
