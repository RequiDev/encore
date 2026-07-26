/**
 * The bar across the top of every signed-in page.
 *
 * It carries the instrument's status rather than its navigation: the wordmark on
 * small screens where the rail is hidden, a quick way into search, whether the
 * last sync worked, the theme control and the account menu. Page-specific
 * controls — the range picker above all — belong to the page's own header,
 * where they sit next to the h1 they qualify.
 */

import type { ReactElement } from 'react'
import { Link } from 'react-router-dom'
import { useSession } from '../../lib/session'
import { formatRelative } from '../../lib/format'
import { Icon } from '../ui/Icon'
import { ThemeToggle } from './ThemeToggle'
import { UserMenu } from './UserMenu'
import { Wordmark } from './Sidebar'

export function TopBar(): ReactElement {
  const { spotify } = useSession()

  return (
    <header className="sticky top-0 z-20 flex h-14 shrink-0 items-center gap-3 border-b border-seam bg-chassis/95 px-3 backdrop-blur sm:px-4">
      <Link to="/" className="rounded-control lg:hidden" aria-label="Encore, dashboard">
        <Wordmark />
      </Link>

      <div className="flex-1" />

      <SyncIndicator
        state={spotify?.syncState ?? null}
        lastSyncAt={spotify?.lastSyncAt ?? null}
        connected={spotify?.connected ?? false}
      />

      <Link
        to="/search"
        className="btn h-8 w-8 border-transparent p-0 text-ink-muted hover:text-ink"
        aria-label="Search the catalogue"
        title="Search"
      >
        <Icon name="search" />
      </Link>

      <ThemeToggle />
      <UserMenu />
    </header>
  )
}

function SyncIndicator({
  state,
  lastSyncAt,
  connected,
}: {
  state: 'ok' | 'needs_reauth' | 'error' | null
  lastSyncAt: string | null
  connected: boolean
}): ReactElement | null {
  if (!state || !connected) return null

  const tone = state === 'ok' ? 'bg-sage' : state === 'needs_reauth' ? 'bg-ember' : 'bg-lamp'
  const text =
    state === 'ok'
      ? lastSyncAt
        ? `Synced ${formatRelative(lastSyncAt)}`
        : 'Sync pending'
      : state === 'needs_reauth'
        ? 'Spotify needs reconnecting'
        : 'Last sync failed'

  return (
    <p className="hidden items-center gap-2 text-xs text-ink-muted md:flex">
      <span className={`h-1.5 w-1.5 rounded-full ${tone}`} aria-hidden="true" />
      {text}
    </p>
  )
}
