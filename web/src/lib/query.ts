/**
 * TanStack Query wiring: one client, one retry policy, one place that notices a
 * session has expired.
 *
 * The 401 handling is here rather than in each hook because a session can lapse
 * during any request — a background refetch of the dashboard just as much as a
 * deliberate click — and every one of those must land the person on the login
 * screen rather than on a page full of error panels.
 */

import { MutationCache, QueryCache, QueryClient } from '@tanstack/react-query'
import { ApiError } from './api'
import type { DateRange } from './range'
import type { SpotifyTimeRange, TopDiffKind } from './types'

/**
 * Retries only what is worth retrying. A 4xx is a statement about the request
 * and will fail identically the second time; a network blip or a 5xx will not.
 */
function shouldRetry(failureCount: number, error: unknown): boolean {
  if (error instanceof ApiError && error.status >= 400 && error.status < 500) return false
  return failureCount < 2
}

type Listener = () => void

const unauthenticatedListeners = new Set<Listener>()

/**
 * Registers interest in "the session has gone". Returns the unsubscribe
 * function, so a component can hand it straight back from `useEffect`.
 */
export function onUnauthenticated(listener: Listener): () => void {
  unauthenticatedListeners.add(listener)
  return () => {
    unauthenticatedListeners.delete(listener)
  }
}

/** Announces that a request came back 401. Exported for the session provider's tests. */
export function notifyUnauthenticated(): void {
  for (const listener of unauthenticatedListeners) listener()
}

function inspect(error: unknown): void {
  if (error instanceof ApiError && error.isUnauthenticated) notifyUnauthenticated()
}

/**
 * Builds the application's query client.
 *
 * `staleTime` is a minute: listening statistics change when a sync or an import
 * commits, not continuously, so refetching on every remount would cost the
 * server real analytic queries for an answer that has not moved.
 */
export function createQueryClient(): QueryClient {
  return new QueryClient({
    queryCache: new QueryCache({ onError: inspect }),
    mutationCache: new MutationCache({ onError: inspect }),
    defaultOptions: {
      queries: {
        retry: shouldRetry,
        staleTime: 60_000,
        gcTime: 5 * 60_000,
        refetchOnWindowFocus: false,
        refetchOnReconnect: true,
      },
      mutations: { retry: false },
    },
  })
}

// --- query keys ------------------------------------------------------------

/** Pagination as it appears in a cache key. */
export interface PageKey {
  limit: number
  offset: number
}

/**
 * Every cache key in one place.
 *
 * Sharing this matters more than it looks: after an import finishes, the imports
 * page invalidates `qk.stats()` and every statistic in the cache goes stale at
 * once. That only works if nobody spells a key by hand.
 */
export const qk = {
  me: () => ['me'] as const,
  status: () => ['status'] as const,
  shares: () => ['shares'] as const,
  playlists: () => ['playlists'] as const,
  sharedStats: (token: string) => ['share', token] as const,
  users: () => ['users'] as const,

  admin: () => ['admin'] as const,
  adminSettings: () => ['admin', 'settings'] as const,
  adminUsers: (page: PageKey) => ['admin', 'users', page] as const,

  stats: () => ['stats'] as const,
  summary: (range: DateRange) => ['stats', 'summary', range] as const,
  top: (kind: 'tracks' | 'artists' | 'albums', range: DateRange, page: PageKey) =>
    ['stats', 'top', kind, range, page] as const,
  topDiff: (kind: TopDiffKind, range: SpotifyTimeRange) => ['stats', 'top-diff', kind, range] as const,
  timeline: (range: DateRange, interval: string | null) =>
    ['stats', 'timeline', range, interval] as const,
  repartition: (kind: 'hour' | 'weekday' | 'heatmap', range: DateRange) =>
    ['stats', 'repartition', kind, range] as const,
  listeningSessions: (range: DateRange, limit: number) =>
    ['stats', 'sessions', range, limit] as const,
  discovery: (range: DateRange, interval: string | null) =>
    ['stats', 'discovery', range, interval] as const,
  genres: (range: DateRange, page: PageKey) => ['stats', 'genres', range, page] as const,
  genreTimeline: (range: DateRange, interval: string | null, genres: string[]) =>
    ['stats', 'genres', 'timeline', range, interval, genres] as const,
  playbackContext: (range: DateRange) => ['stats', 'context', range] as const,
  taste: (range: DateRange) => ['stats', 'taste', range] as const,
  streaks: (range: DateRange) => ['stats', 'streaks', range] as const,
  extras: (range: DateRange) => ['stats', 'extras', range] as const,
  // Keyed by range even though one of the three lists ignores it: the other
  // two do not, so a range change must still be a cache miss.
  library: (range: DateRange) => ['stats', 'library', range] as const,
  compare: (a: DateRange, b: DateRange) => ['stats', 'compare', a, b] as const,
  yearInReview: (year: number) => ['stats', 'year', year] as const,
  affinity: (userId: string, range: DateRange) => ['stats', 'affinity', userId, range] as const,

  track: (id: string, range: DateRange) => ['entity', 'track', id, range] as const,
  artist: (id: string, range: DateRange) => ['entity', 'artist', id, range] as const,
  album: (id: string, range: DateRange) => ['entity', 'album', id, range] as const,
  // Deliberately not keyed by range: the listing and "have you ever played
  // this" are both all-time, exactly like the completion figure beside them.
  albumTracklist: (id: string) => ['entity', 'album', id, 'tracklist'] as const,
  search: (query: string, limit: number) => ['search', query, limit] as const,

  history: (range: DateRange, limit: number) => ['history', range, limit] as const,
  blacklist: () => ['blacklist'] as const,

  imports: () => ['imports'] as const,
  importList: (page: PageKey) => ['imports', 'list', page] as const,
  importJob: (id: string) => ['imports', 'job', id] as const,
  importRejects: (id: string, page: PageKey) => ['imports', 'rejects', id, page] as const,
} as const
