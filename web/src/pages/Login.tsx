/**
 * The sign-in screen.
 *
 * Spotify is the only identity provider, so there is no form here — the whole
 * page is one link that leaves for the OAuth flow. It is an anchor rather than a
 * button because the server answers with a redirect, and a `fetch` would follow
 * it into a CORS wall.
 *
 * The failure cases are named rather than lumped into "something went wrong":
 * "this instance is not accepting new accounts" is actionable and "sign-in
 * failed" is not.
 */

import type { ReactElement } from 'react'
import { Navigate, useLocation, useSearchParams } from 'react-router-dom'
import { Wordmark } from '../components/layout/Sidebar'
import { ThemeToggle } from '../components/layout/ThemeToggle'
import { Icon } from '../components/ui/Icon'
import { Spinner } from '../components/ui/Spinner'
import { useDocumentTitle } from '../lib/hooks'
import { useSession } from '../lib/session'

const ERROR_MESSAGES: Record<string, string> = {
  registrations_disabled:
    'This instance is not accepting new accounts. Ask whoever runs it to enable registrations, or sign in with an account that already exists here.',
  account_disabled: 'This account has been deactivated by an administrator.',
  access_denied: 'Spotify sign-in was cancelled before it finished.',
  invalid_state:
    'That sign-in attempt had expired, or had already been used. Starting again will fix it.',
  csrf: 'That sign-in attempt could not be verified. Starting again will fix it.',
  rate_limited: 'Too many attempts in a short time. Wait a moment and try again.',
  // Deliberately does not say "try again": for this one, trying again is the
  // only thing that cannot help.
  spotify_rate_limited:
    'Spotify is currently rate limiting this instance, so it could not verify the sign-in. This clears on its own — usually within a day — and no listening data is affected. Whoever runs this instance can see when it lifts under Settings → Music metadata.',
}

function messageFor(code: string): string {
  return ERROR_MESSAGES[code] ?? 'Sign-in did not complete. Trying again usually works.'
}

/**
 * Where to return to after signing in. Only a path from this application's own
 * navigation state is used, never a value from the query string, so the link
 * cannot be turned into an open redirect.
 */
function safeReturnPath(state: unknown): string {
  if (typeof state !== 'object' || state === null) return '/'
  const from = (state as { from?: unknown }).from
  if (typeof from !== 'string') return '/'
  if (!from.startsWith('/') || from.startsWith('//')) return '/'
  if (from.startsWith('/login')) return '/'
  return from
}

export default function Login(): ReactElement {
  const { isLoading, isAuthenticated } = useSession()
  const location = useLocation()
  const [params] = useSearchParams()
  useDocumentTitle('Sign in')

  const returnTo = safeReturnPath(location.state)
  const error = params.get('error')

  if (isLoading) {
    return (
      <div className="flex min-h-dvh items-center justify-center bg-chassis">
        <Spinner size={20} label="Checking your session" />
      </div>
    )
  }

  if (isAuthenticated) return <Navigate to={returnTo} replace />

  const loginUrl = `/api/auth/spotify/login?redirect_to=${encodeURIComponent(
    `${window.location.origin}${returnTo}`,
  )}`

  return (
    <div className="flex min-h-dvh flex-col bg-chassis">
      <div className="flex justify-end p-4">
        <ThemeToggle />
      </div>

      <div className="flex flex-1 items-center justify-center px-4 pb-16">
        <div className="w-full max-w-md">
          <Wordmark className="justify-center" />

          <h1 className="mt-8 text-center text-2xl font-semibold tracking-tight text-ink">
            Your listening history, kept by you
          </h1>
          <p className="mx-auto mt-3 max-w-sm text-center text-sm text-ink-muted">
            Encore records what you play, imports the years Spotify already has, and answers
            questions about it. Everything stays on this server.
          </p>

          {error ? (
            <div
              role="alert"
              className="panel mt-6 flex items-start gap-3 border-ember px-3.5 py-3"
            >
              <span className="mt-0.5 shrink-0 text-ember">
                <Icon name="warning" />
              </span>
              <div className="min-w-0">
                <p className="text-sm text-ink">{messageFor(error)}</p>
                <p className="tabular mt-1 text-xs text-ink-faint">{error}</p>
              </div>
            </div>
          ) : null}

          <a href={loginUrl} className="btn btn-primary mt-8 w-full">
            Continue with Spotify
            <Icon name="external" size={14} />
          </a>

          <p className="mt-4 text-center text-xs text-ink-faint">
            Encore asks Spotify for read access to your profile, your listening history and
            library, and what you&rsquo;re playing now. It writes to your account only if you ask
            it to build a playlist, and only to playlists it created itself.
          </p>
        </div>
      </div>
    </div>
  )
}
