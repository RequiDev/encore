/**
 * Asks an account whose Spotify grant predates a scope increase to relink.
 *
 * A refresh token carries the grant it was issued with forever, so every
 * account that existed before Encore started asking for more stays short
 * until it relinks — and would otherwise only discover that as a 403 on
 * whichever feature needed the missing scope. This says so up front instead,
 * in plain language, and never as a blocker: an account that ignores it keeps
 * working exactly as before, minus the features it has not granted.
 *
 * The dismissal is keyed by the *set* of missing scopes rather than by a
 * plain flag, so dismissing today's prompt cannot silently swallow a
 * different one a later phase adds — the banner only stays quiet for the
 * exact shortfall it was dismissed for.
 */

import type { ReactElement } from 'react'
import { useState } from 'react'
import { useSession } from '../../lib/session'
import { Icon } from '../ui/Icon'

const DISMISSED_STORAGE_KEY = 'encore.reconsent.dismissed'

/**
 * What each scope buys, for somebody deciding whether to click. A scope this
 * phase does not know about — a future one — falls back to its raw name
 * rather than vanishing, so the banner never quietly says nothing about a
 * permission it is in fact asking for.
 */
const SCOPE_EXPLANATIONS: Record<string, string> = {
  'user-top-read': "compare Spotify's own ranking to yours",
  'user-library-read': 'see what you saved but never played',
  'user-follow-read': 'see which artists you follow but stopped playing',
  'playlist-read-private': 'name the playlist a listen came from',
  'user-read-playback-state': "show what's playing now",
}

function explain(scope: string): string {
  return SCOPE_EXPLANATIONS[scope] ?? scope
}

/** A stable key for a set of scopes, independent of the order the server lists them in. */
function scopeSetKey(scopes: string[]): string {
  return [...scopes].sort().join(' ')
}

/** Anything unreadable — storage disabled, private browsing — means "not dismissed". */
function readDismissedKey(): string | null {
  try {
    return window.localStorage.getItem(DISMISSED_STORAGE_KEY)
  } catch {
    return null
  }
}

function writeDismissedKey(key: string): void {
  try {
    window.localStorage.setItem(DISMISSED_STORAGE_KEY, key)
  } catch {
    // Private browsing with storage disabled: the dismissal applies to this
    // tab only, which is a smaller problem than refusing to let it dismiss.
  }
}

export function ReconsentBanner(): ReactElement | null {
  const { spotify } = useSession()
  const missingScopes = spotify?.missingScopes ?? []

  // Lazy-initialised from storage once; updated directly by the dismiss
  // handler below rather than through an effect, so there is no render where
  // a freshly loaded grant is reconciled against a stale flag.
  const [dismissedKey, setDismissedKey] = useState(readDismissedKey)

  // No Spotify connection at all is a different, already-handled state — not
  // "every scope is missing" — so an account with no connection sees nothing
  // here regardless of what the server would compute for it.
  if (!spotify?.connected || missingScopes.length === 0) return null

  const key = scopeSetKey(missingScopes)
  if (dismissedKey === key) return null

  return (
    <div role="region" aria-labelledby="reconsent-heading" className="border-b border-seam bg-panel">
      <div className="mx-auto flex max-w-[1600px] flex-col gap-3 px-3 py-3 sm:flex-row sm:items-start sm:px-5">
        <span className="mt-0.5 shrink-0 text-lamp" aria-hidden="true">
          <Icon name="info" size={18} />
        </span>

        <div className="min-w-0 flex-1">
          <p id="reconsent-heading" className="eyebrow">
            Spotify permissions
          </p>
          <p className="mt-1 max-w-prose text-sm text-ink">
            Encore has gained a few statistics that need read access it did not previously
            request. Reconnecting with Spotify lets it:
          </p>
          <ul className="mt-2 list-disc space-y-1 pl-5 text-sm text-ink">
            {missingScopes.map((scope) => (
              <li key={scope}>{explain(scope)}</li>
            ))}
          </ul>
          <p className="mt-2 max-w-prose text-sm text-ink-muted">
            None of these let Encore change anything on your Spotify account.
          </p>
        </div>

        <div className="flex shrink-0 items-center gap-2 self-end sm:self-start">
          <a
            // A full navigation, not a fetch: the server answers with a
            // redirect to Spotify's authorisation page.
            href="/api/auth/spotify/relink"
            className="btn btn-primary text-sm"
          >
            <Icon name="refresh" />
            Reconnect Spotify
          </a>
          <button
            type="button"
            onClick={() => {
              writeDismissedKey(key)
              setDismissedKey(key)
            }}
            className="btn h-8 w-8 border-transparent p-0 text-ink-muted hover:text-ink"
            aria-label="Dismiss"
            title="Dismiss"
          >
            <Icon name="close" size={14} />
          </button>
        </div>
      </div>
    </div>
  )
}
