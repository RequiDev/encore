/**
 * Who is signed in.
 *
 * `GET /api/me` is fetched once and cached; every part of the shell reads it
 * from here rather than asking again. A 401 is not an error in this module — it
 * is the ordinary "nobody is signed in" answer — so it resolves to null and lets
 * `RequireAuth` do the redirecting. That distinction is what keeps a logged-out
 * visitor from seeing an error panel where a login screen belongs.
 */

import type { ReactElement, ReactNode } from 'react'
import { createContext, use, useCallback, useEffect, useMemo } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Navigate, Outlet, useLocation } from 'react-router-dom'
import { ApiError, api } from './api'
import { onUnauthenticated, qk } from './query'
import type {
  InstanceInfo,
  ListeningBounds,
  MeResponse,
  SpotifyConnection,
  User,
} from './types'

export interface SessionValue {
  user: User | null
  spotify: SpotifyConnection | null
  instance: InstanceInfo | null
  /** The span of history the user holds; both nulls before they import anything. */
  listening: ListeningBounds | null
  /** True until the bootstrap call has settled, once, on first load. */
  isLoading: boolean
  isAuthenticated: boolean
  /** True when the caller is an active administrator. */
  isAdmin: boolean
  /** Re-reads `/api/me`, for after a timezone change or a Spotify relink. */
  refresh: () => Promise<void>
  logout: () => Promise<void>
}

const SessionContext = createContext<SessionValue | null>(null)

async function fetchMe(): Promise<MeResponse | null> {
  try {
    return await api.get<MeResponse>('/me')
  } catch (error) {
    // Signed out is a state, not a failure. Anything else is a real problem and
    // is allowed to propagate so the error boundary can show it.
    if (error instanceof ApiError && error.isUnauthenticated) return null
    throw error
  }
}

/**
 * Loads the session once and keeps it available to the tree.
 *
 * It also listens for a 401 arriving from anywhere else in the application: when
 * one does, the cached session is dropped, which flips `isAuthenticated` and
 * sends the guards to the login page. Doing it through the cache rather than by
 * navigating directly means a background refetch cannot yank the page out from
 * under someone mid-interaction on a route that does not need a session.
 */
export function SessionProvider({ children }: { children: ReactNode }): ReactElement {
  const queryClient = useQueryClient()

  const query = useQuery({
    queryKey: qk.me(),
    queryFn: fetchMe,
    retry: false,
    staleTime: 5 * 60_000,
    refetchOnWindowFocus: true,
  })

  useEffect(
    () =>
      onUnauthenticated(() => {
        queryClient.setQueryData(qk.me(), null)
      }),
    [queryClient],
  )

  const refresh = useCallback(async () => {
    await queryClient.invalidateQueries({ queryKey: qk.me() })
  }, [queryClient])

  const logout = useCallback(async () => {
    try {
      await api.post<void>('/auth/logout')
    } catch {
      // The cookie may already be gone, or the server may be unreachable. Either
      // way the right local outcome is the same: forget everything we hold.
    }
    queryClient.setQueryData(qk.me(), null)
    queryClient.removeQueries({ predicate: (q) => q.queryKey[0] !== 'me' })
  }, [queryClient])

  const me = query.data ?? null

  const value = useMemo<SessionValue>(
    () => ({
      user: me?.user ?? null,
      spotify: me?.spotify ?? null,
      instance: me?.instance ?? null,
      listening: me?.listening ?? null,
      isLoading: query.isPending,
      isAuthenticated: me !== null,
      isAdmin: me?.user.role === 'admin' && me.user.isActive,
      refresh,
      logout,
    }),
    [me, query.isPending, refresh, logout],
  )

  return <SessionContext value={value}>{children}</SessionContext>
}

/** The current session. Throws outside a `SessionProvider`, which is a wiring bug. */
export function useSession(): SessionValue {
  const value = use(SessionContext)
  if (!value) throw new Error('useSession must be used inside a SessionProvider')
  return value
}

/**
 * The timezone every date in the interface is rendered in.
 *
 * It is the user's configured zone, because that is the one the server bucketed
 * the statistics in. Before the session loads — and on the login screen, which
 * has none — the browser's own zone is a better guess than UTC.
 */
/**
 * The span of history the signed-in user holds, or null when it is not known
 * yet. Returns the raw bounds and applies no fallback: what a missing bound
 * should mean is the caller's decision, and lib/range owns that.
 *
 * This deliberately does not import lib/range. That module already imports this
 * one for the user's timezone, and closing the loop would make the two mutually
 * dependent for a single constant.
 */
export function useListeningBounds(): ListeningBounds | null {
  const session = use(SessionContext)
  return session?.listening ?? null
}

export function useTimeZone(): string {
  const session = use(SessionContext)
  const configured = session?.user?.timezone
  if (configured) return configured
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
  } catch {
    return 'UTC'
  }
}

/** Full-height placeholder shown while the bootstrap call is in flight. */
function SessionPending(): ReactElement {
  return (
    <div
      className="flex min-h-dvh items-center justify-center"
      role="status"
      aria-live="polite"
      aria-busy="true"
    >
      <span className="eyebrow">Loading Encore</span>
    </div>
  )
}

/**
 * Gate for everything behind a session. Renders its children, or the matched
 * child route when used as a layout element.
 *
 * The attempted location is carried in navigation state so that signing in
 * returns the person to the page they asked for rather than to the dashboard.
 */
export function RequireAuth({ children }: { children?: ReactNode }): ReactElement {
  const { isLoading, isAuthenticated } = useSession()
  const location = useLocation()

  if (isLoading) return <SessionPending />
  if (!isAuthenticated) {
    return <Navigate to="/login" state={{ from: location.pathname + location.search }} replace />
  }
  return <>{children ?? <Outlet />}</>
}

/**
 * Gate for the administration page. A non-administrator is sent to the ordinary
 * settings page rather than shown a refusal, because on a self-hosted instance
 * the usual reason to arrive here is a stale bookmark from before a demotion.
 */
export function RequireAdmin({ children }: { children?: ReactNode }): ReactElement {
  const { isLoading, isAuthenticated, isAdmin } = useSession()
  const location = useLocation()

  if (isLoading) return <SessionPending />
  if (!isAuthenticated) {
    return <Navigate to="/login" state={{ from: location.pathname + location.search }} replace />
  }
  if (!isAdmin) return <Navigate to="/settings" replace />
  return <>{children ?? <Outlet />}</>
}
