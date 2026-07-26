/**
 * Who is signed in, and the few things they can do about it.
 *
 * The Spotify connection state lives here rather than on a settings page,
 * because a lapsed refresh token stops new listens arriving and the person needs
 * to find out without going looking. `needs_reauth` is the one state the menu
 * shouts about.
 */

import type { ReactElement } from 'react'
import { useRef } from 'react'
import { Link } from 'react-router-dom'
import { useEscapeKey, useOnClickOutside, useToggle } from '../../lib/hooks'
import { formatRelative } from '../../lib/format'
import { useSession } from '../../lib/session'
import { Chip } from '../ui/Chip'
import { Icon } from '../ui/Icon'

export function UserMenu(): ReactElement | null {
  const { user, spotify, isAdmin, logout } = useSession()
  const menu = useToggle(false)
  const container = useRef<HTMLDivElement>(null)

  useOnClickOutside(container, menu.on, menu.close)
  useEscapeKey(menu.on, menu.close)

  if (!user) return null

  const needsReauth = spotify?.syncState === 'needs_reauth'
  const syncFailed = spotify?.syncState === 'error'

  return (
    <div ref={container} className="relative">
      <button
        type="button"
        onClick={menu.toggle}
        aria-expanded={menu.on}
        aria-controls="account-menu"
        className="btn h-8 gap-2 border-transparent pr-2 pl-1.5 text-ink-muted hover:text-ink"
      >
        <Avatar url={user.avatarUrl} name={user.displayName} />
        <span className="hidden max-w-32 truncate text-sm sm:inline">{user.displayName}</span>
        {needsReauth ? (
          <span className="h-1.5 w-1.5 rounded-full bg-ember" aria-hidden="true" />
        ) : null}
        <Icon name="chevron-down" size={14} />
      </button>

      {menu.on ? (
        // A disclosure rather than an ARIA menu: these are ordinary links, and
        // an element that claims role="menu" owes the keyboard arrow-key
        // navigation that a menu implies.
        <div
          id="account-menu"
          aria-label="Account"
          className="panel-raised absolute top-full right-0 z-30 mt-2 w-64 py-1"
        >
          <div className="border-b border-seam px-3 pt-2 pb-3">
            <p className="truncate text-sm font-medium text-ink">{user.displayName}</p>
            <p className="tabular truncate text-xs text-ink-faint">{user.spotifyUserId}</p>
            <div className="mt-2 flex flex-wrap items-center gap-1.5">
              {isAdmin ? <Chip tone="lamp">Admin</Chip> : null}
              {needsReauth ? (
                <Chip tone="bad">Reconnect needed</Chip>
              ) : syncFailed ? (
                <Chip tone="warn">Sync error</Chip>
              ) : spotify?.connected ? (
                <Chip tone="good">Connected</Chip>
              ) : (
                <Chip>Not connected</Chip>
              )}
            </div>
            {spotify?.lastSyncAt ? (
              <p className="mt-2 text-xs text-ink-faint">
                Last sync {formatRelative(spotify.lastSyncAt)}
              </p>
            ) : null}
          </div>

          <MenuLink to="/settings" icon="settings" onClick={menu.close}>
            Settings
          </MenuLink>
          {isAdmin ? (
            <MenuLink to="/settings/admin" icon="admin" onClick={menu.close}>
              Administration
            </MenuLink>
          ) : null}
          {needsReauth ? (
            <a
              // A full navigation, not a fetch: the server answers with a 302
              // to Spotify's authorisation page.
              href="/api/auth/spotify/relink"
              className="flex items-center gap-2.5 px-3 py-2 text-sm text-lamp hover:bg-panel"
            >
              <Icon name="refresh" />
              Reconnect Spotify
            </a>
          ) : null}
          <button
            type="button"
            onClick={() => {
              menu.close()
              void logout()
            }}
            className="flex w-full items-center gap-2.5 px-3 py-2 text-sm text-ink-muted hover:bg-panel hover:text-ink"
          >
            <Icon name="logout" />
            Sign out
          </button>
        </div>
      ) : null}
    </div>
  )
}

function MenuLink({
  to,
  icon,
  onClick,
  children,
}: {
  to: string
  icon: 'settings' | 'admin'
  onClick: () => void
  children: string
}): ReactElement {
  return (
    <Link
      to={to}
      onClick={onClick}
      className="flex items-center gap-2.5 px-3 py-2 text-sm text-ink-muted hover:bg-panel hover:text-ink"
    >
      <Icon name={icon} />
      {children}
    </Link>
  )
}

function Avatar({ url, name }: { url: string; name: string }): ReactElement {
  if (url) {
    return (
      <img
        src={url}
        // The name is already beside it in the button, so repeating it here
        // would have a screen reader say it twice.
        alt=""
        width={22}
        height={22}
        className="h-[22px] w-[22px] rounded-full object-cover"
      />
    )
  }
  const initial = name.trim().charAt(0).toUpperCase() || '?'
  return (
    <span
      aria-hidden="true"
      className="flex h-[22px] w-[22px] items-center justify-center rounded-full bg-seam text-[0.625rem] font-semibold text-ink-muted"
    >
      {initial}
    </span>
  )
}
