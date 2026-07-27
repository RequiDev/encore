/**
 * The sidebar, and the two shapes it takes on a small screen.
 *
 * Above `lg` it is a permanent rail. Below it, the four main destinations move
 * into a bottom bar that a thumb can reach, and everything else lives behind
 * "More", which opens the same navigation as a drawer. One list of links, three
 * presentations — see `nav.ts`.
 */

import type { ReactElement } from 'react'
import { useEffect, useRef } from 'react'
import { NavLink, useLocation } from 'react-router-dom'
import { useEscapeKey, useFocusTrap, useScrollLock } from '../../lib/hooks'
import { Icon } from '../ui/Icon'
import { NAV_SECTIONS, PRIMARY_NAV } from './nav'
import type { NavItem } from './nav'

function linkClass({ isActive }: { isActive: boolean }): string {
  return [
    'flex items-center gap-2.5 rounded-control px-2.5 py-1.5 text-sm transition-colors',
    isActive
      ? // The active rail is the one place amber appears in the chrome: a lit
        // indicator against an unlit row of them.
        'bg-panel-raised text-ink'
      : 'text-ink-muted hover:bg-panel-raised hover:text-ink',
  ].join(' ')
}

function NavRow({ item, onNavigate }: { item: NavItem; onNavigate?: () => void }): ReactElement {
  return (
    <li>
      <NavLink to={item.to} end={item.exact} className={linkClass} onClick={onNavigate}>
        {({ isActive }) => (
          <>
            <span
              aria-hidden="true"
              className={`h-4 w-px shrink-0 ${isActive ? 'bg-lamp' : 'bg-transparent'}`}
            />
            <Icon name={item.icon} />
            <span className="truncate">{item.label}</span>
          </>
        )}
      </NavLink>
    </li>
  )
}

export interface NavListProps {
  /** Called after a link is followed, so the drawer can close itself. */
  onNavigate?: () => void
  className?: string
}

/** The full navigation, shared by the rail and the drawer. */
export function NavList({ onNavigate, className }: NavListProps): ReactElement {
  return (
    <nav aria-label="Main" className={['space-y-5', className].filter(Boolean).join(' ')}>
      {NAV_SECTIONS.map((section) => (
        <div key={section.title}>
          <p className="eyebrow px-2.5 pb-1.5">{section.title}</p>
          <ul className="space-y-0.5">
            {section.items.map((item) => (
              <NavRow key={item.to} item={item} onNavigate={onNavigate} />
            ))}
          </ul>
        </div>
      ))}
    </nav>
  )
}

/** The Encore mark and wordmark, used in the rail, the drawer and on the login screen. */
export function Wordmark({ className }: { className?: string }): ReactElement {
  return (
    <span className={['flex items-center gap-2.5', className].filter(Boolean).join(' ')}>
      <svg width="22" height="22" viewBox="0 0 24 24" aria-hidden="true" focusable="false">
        <rect x="1" y="1" width="22" height="22" rx="5" className="fill-panel-raised" />
        <path
          d="M6 16.5a8 8 0 0 1 12 0"
          fill="none"
          stroke="currentColor"
          strokeOpacity="0.35"
          strokeWidth="1.4"
          strokeLinecap="round"
        />
        <path
          d="M12 16.5 16.2 9.6"
          fill="none"
          className="stroke-lamp"
          strokeWidth="1.6"
          strokeLinecap="round"
        />
        <circle cx="12" cy="16.5" r="1.5" className="fill-lamp" />
      </svg>
      <span className="text-sm font-semibold tracking-[0.18em] text-ink uppercase">Encore</span>
    </span>
  )
}

export interface SidebarProps {
  /** Instance version, shown at the foot of the rail. */
  version?: string
}

/** The permanent rail, from `lg` upwards. */
export function Sidebar({ version }: SidebarProps): ReactElement {
  return (
    <aside className="hidden w-60 shrink-0 flex-col border-r border-seam bg-panel lg:flex">
      <div className="flex h-14 items-center border-b border-seam px-4">
        <Wordmark />
      </div>
      <div className="flex-1 overflow-y-auto p-3">
        <NavList />
      </div>
      {version ? (
        <div className="border-t border-seam px-4 py-2.5">
          {/*
            The published tag is `main-<full sha>`, far wider than a 15rem rail.
            Truncated rather than wrapped — three lines of hexadecimal at the
            foot of the navigation is worse than an ellipsis — with the whole
            value on the title so it can still be read and copied.
          */}
          <p className="eyebrow flex min-w-0 items-baseline gap-1.5" title={version}>
            <span className="shrink-0">Version</span>
            <span className="tabular min-w-0 truncate">{version}</span>
          </p>
        </div>
      ) : null}
    </aside>
  )
}

export interface BottomNavProps {
  onOpenDrawer: () => void
  drawerOpen: boolean
}

/** The phone bar: four destinations and everything else. */
export function BottomNav({ onOpenDrawer, drawerOpen }: BottomNavProps): ReactElement {
  return (
    <nav
      aria-label="Primary"
      className="fixed inset-x-0 bottom-0 z-30 border-t border-seam bg-panel lg:hidden"
      style={{ paddingBottom: 'env(safe-area-inset-bottom)' }}
    >
      <ul className="grid grid-cols-5">
        {PRIMARY_NAV.map((item) => (
          <li key={item.to}>
            <NavLink
              to={item.to}
              end={item.exact}
              className={({ isActive }) =>
                [
                  'flex flex-col items-center gap-1 py-2 text-[0.625rem] font-medium',
                  isActive ? 'text-lamp' : 'text-ink-muted',
                ].join(' ')
              }
            >
              <Icon name={item.icon} size={18} />
              <span>{item.label}</span>
            </NavLink>
          </li>
        ))}
        <li>
          <button
            type="button"
            onClick={onOpenDrawer}
            aria-expanded={drawerOpen}
            aria-haspopup="dialog"
            className="flex w-full flex-col items-center gap-1 py-2 text-[0.625rem] font-medium text-ink-muted"
          >
            <Icon name="more" size={18} />
            <span>More</span>
          </button>
        </li>
      </ul>
    </nav>
  )
}

export interface NavDrawerProps {
  open: boolean
  onClose: () => void
  version?: string
}

/** Everything the bottom bar could not hold, as a dismissible sheet. */
export function NavDrawer({ open, onClose, version }: NavDrawerProps): ReactElement | null {
  const panel = useRef<HTMLDivElement>(null)
  const location = useLocation()

  useFocusTrap(panel, open)
  useEscapeKey(open, onClose)
  useScrollLock(open)

  // A drawer left open across a navigation — a browser Back, say — would cover
  // the page the person just asked for, so any route change closes it. The
  // component stays mounted while closed, so this runs on the path changing
  // rather than on the drawer opening.
  useEffect(() => {
    onClose()
  }, [location.pathname, onClose])

  if (!open) return null

  return (
    <div className="fixed inset-0 z-40 lg:hidden">
      <button
        type="button"
        aria-label="Close navigation"
        onClick={onClose}
        className="absolute inset-0 bg-chassis/70"
      />
      <div
        ref={panel}
        role="dialog"
        aria-modal="true"
        aria-label="Navigation"
        tabIndex={-1}
        className="absolute inset-y-0 left-0 flex w-72 max-w-[85vw] flex-col border-r border-seam bg-panel"
      >
        <div className="flex h-14 items-center justify-between border-b border-seam px-4">
          <Wordmark />
          <button
            type="button"
            onClick={onClose}
            aria-label="Close navigation"
            className="rounded-control p-1.5 text-ink-muted hover:text-ink"
          >
            <Icon name="close" size={18} />
          </button>
        </div>
        <div className="flex-1 overflow-y-auto p-3">
          <NavList onNavigate={onClose} />
        </div>
        {version ? (
          <div className="border-t border-seam px-4 py-2.5">
            <p className="eyebrow flex min-w-0 items-baseline gap-1.5" title={version}>
              <span className="shrink-0">Version</span>
              <span className="tabular min-w-0 truncate">{version}</span>
            </p>
          </div>
        ) : null}
      </div>
    </div>
  )
}
