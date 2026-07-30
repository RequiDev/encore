/**
 * The playlist/album context section of the Habits page.
 *
 * This figure has the narrowest coverage of anything in the app: context_type
 * and context_id are written only by live sync, never by any Spotify export,
 * so a fresh instance and an import-only one both read zero forever, and even
 * a synced instance only ever covers the slice of listening sync itself saw.
 * These tests pin the four states the brief calls out explicitly rather than
 * the happy path alone, because the three non-happy ones are the point: a
 * user seeing a small percentage here must be told it is a property of the
 * data, not a bug, and an unnamed context must never render a raw Spotify id
 * or vanish from the total its own coverage figure promises.
 */

import type { ReactElement } from 'react'
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryRouter } from 'react-router-dom'
import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { routes } from '../App'
import { createQueryClient } from '../lib/query'
import type { MeResponse, PlaybackContextResponse, PlaylistContextEntry } from '../lib/types'

function me(missingScopes: string[] = []): MeResponse {
  return {
    user: {
      id: 'cf0a1e6c-0000-4000-8000-000000000003',
      spotifyUserId: 'someone',
      displayName: 'Someone',
      email: 'someone@example.com',
      avatarUrl: '',
      role: 'user',
      isActive: true,
      timezone: 'UTC',
      createdAt: '2026-01-04T10:00:00Z',
      lastLoginAt: '2026-07-26T08:12:00Z',
    },
    spotify: {
      connected: true,
      syncState: 'ok',
      lastSyncAt: '2026-07-26T08:11:03Z',
      lastSyncError: '',
      scopes: ['user-read-recently-played'],
      missingScopes,
    },
    csrfToken: 'not-a-real-token',
    listening: { firstListenAt: null, lastListenAt: null },
    instance: { registrationsEnabled: false, version: '1.0.0' },
  }
}

function playlistEntry(overrides: Partial<PlaylistContextEntry> = {}): PlaylistContextEntry {
  return { contextType: 'playlist', contextId: 'pl-1', name: 'Road Trip', plays: 10, ...overrides }
}

/**
 * A full context payload with every rate at zero coverage by default, so a
 * test that only cares about the playlist section is not also asserting
 * anything about the six extended-export figures above it.
 */
function contextPayload(overrides: Partial<PlaybackContextResponse> = {}): PlaybackContextResponse {
  const zeroRate = { value: 0, covered: 0, total: 0 }
  const zeroCoverage = { covered: 0, total: 0 }
  return {
    endReasons: [],
    endReasonCoverage: zeroCoverage,
    skipRate: zeroRate,
    shuffleRate: zeroRate,
    platforms: [],
    platformCoverage: zeroCoverage,
    countries: [],
    countryCoverage: zeroCoverage,
    offlineRate: zeroRate,
    incognitoRate: zeroRate,
    playlists: [],
    playlistCoverage: zeroCoverage,
    ...overrides,
  }
}

function tastePayload(): unknown {
  const zeroRate = { value: 0, covered: 0, total: 0 }
  return { obscurity: zeroRate, releaseLag: zeroRate }
}

/** Answers each path with its own body, so one page can be given a whole API. */
function stubRoutes(bodies: Record<string, unknown>): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString()
      const path = new URL(url, 'http://encore.test').pathname
      const body = bodies[path]
      if (body === undefined) {
        return new Response(JSON.stringify({ error: { code: 'not_found', message: 'No.' } }), {
          status: 404,
          headers: { 'content-type': 'application/json' },
        })
      }
      return new Response(JSON.stringify(body), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      })
    }),
  )
}

function mountAt(path: string): ReactElement {
  const router = createMemoryRouter(routes, { initialEntries: [path] })
  return (
    <QueryClientProvider client={createQueryClient()}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}

async function waitForPage(): Promise<void> {
  await screen.findByRole('heading', { level: 1, name: 'Habits' })
}

beforeEach(() => {
  vi.unstubAllGlobals()
})

/**
 * The chart's category-axis tick text is Recharts SVG the test environment
 * never measures a size for — `ResponsiveContainer` reports 0×0 in jsdom, so
 * Recharts renders nothing but the accessible caption underneath it. That
 * caption is a real sentence naming the leader and (when there is more than
 * one row) the trailer, so it is what these tests read a bar's label from,
 * exactly as `charts.test.tsx` already does for `BarChart` on its own.
 */
async function findPlaylistChartCaption(): Promise<HTMLElement> {
  return screen.findByText(/^Plays by playlist, album or collection\./)
}

describe('the playlist/album context section of the Habits page', () => {
  it('leads with the denominator and says why an import cannot contribute, when coverage is non-zero', async () => {
    stubRoutes({
      '/api/me': me([]),
      '/api/stats/context': contextPayload({
        playlistCoverage: { covered: 31, total: 1000 },
        playlists: [playlistEntry()],
      }),
      '/api/stats/taste': tastePayload(),
    })

    render(mountAt('/habits'))
    await waitForPage()

    // The exact two-sentence form the brief requires: the denominator first,
    // then the reason an import can never move it. Without the second
    // sentence the number reads as a bug rather than a property of the data.
    expect(
      await screen.findByText(
        "Based on the 3.1% of your listening Encore recorded live. No Spotify export records what you were playing from, so imported history cannot contribute.",
      ),
    ).toBeInTheDocument()

    // Not the zero-coverage empty state: real data is on screen.
    expect(
      screen.queryByText('Encore has not recorded any plays live yet'),
    ).not.toBeInTheDocument()
    expect(await findPlaylistChartCaption()).toHaveTextContent('led by Road Trip with 10')
  })

  it('is its own state — not an empty chart — when nothing has been recorded live yet', async () => {
    stubRoutes({
      '/api/me': me([]),
      '/api/stats/context': contextPayload({
        playlistCoverage: { covered: 0, total: 1000 },
        playlists: [],
      }),
      '/api/stats/taste': tastePayload(),
    })

    render(mountAt('/habits'))
    await waitForPage()

    expect(
      await screen.findByText('Encore has not recorded any plays live yet'),
    ).toBeInTheDocument()
    expect(screen.getByText('This fills in as it syncs.')).toBeInTheDocument()

    // The denominator banner and the ranked chart belong to the "has data"
    // state and must not also render here.
    expect(screen.queryByText(/no spotify export records/i)).not.toBeInTheDocument()
    expect(screen.queryByText('What you were playing from')).not.toBeInTheDocument()
  })

  it('names an unresolved playlist "Unknown playlist", never by its raw Spotify id, and still counts it', async () => {
    stubRoutes({
      '/api/me': me([]),
      '/api/stats/context': contextPayload({
        playlistCoverage: { covered: 1, total: 10 },
        // Deleted since, or never this listener's own: a playlist context
        // with no match in user_playlists.
        playlists: [
          playlistEntry({ contextType: 'playlist', contextId: 'ghost-playlist', name: '', plays: 3 }),
        ],
      }),
      '/api/stats/taste': tastePayload(),
    })

    render(mountAt('/habits'))
    await waitForPage()

    expect(await findPlaylistChartCaption()).toHaveTextContent('led by Unknown playlist with 3')
    // Never a raw Spotify id standing in for a name.
    expect(screen.queryByText('ghost-playlist')).not.toBeInTheDocument()
  })

  it('names Spotify\'s "collection" context "Liked Songs" — unambiguous, and needing no lookup', async () => {
    stubRoutes({
      '/api/me': me([]),
      '/api/stats/context': contextPayload({
        playlistCoverage: { covered: 2, total: 10 },
        // A bare "spotify:collection" URI (no id segment) is coalesced to an
        // empty contextId server-side, per playlistContextSQL's own doc
        // comment — this must still group and render, not break.
        playlists: [playlistEntry({ contextType: 'collection', contextId: '', name: '', plays: 2 })],
      }),
      '/api/stats/taste': tastePayload(),
    })

    render(mountAt('/habits'))
    await waitForPage()

    expect(await findPlaylistChartCaption()).toHaveTextContent('led by Liked Songs with 2')
  })

  it('names an unnamed album from its context type, never by a raw catalogue id', async () => {
    stubRoutes({
      '/api/me': me([]),
      '/api/stats/context': contextPayload({
        playlistCoverage: { covered: 1, total: 10 },
        // Albums are never named by this join, by construction — not
        // because a lookup happened to fail — so this is not an edge case,
        // it is every album context there will ever be.
        playlists: [playlistEntry({ contextType: 'album', contextId: 'alb-9', name: '', plays: 1 })],
      }),
      '/api/stats/taste': tastePayload(),
    })

    render(mountAt('/habits'))
    await waitForPage()

    expect(await findPlaylistChartCaption()).toHaveTextContent('led by An unnamed album with 1')
    expect(screen.queryByText('alb-9')).not.toBeInTheDocument()
  })

  it('says the counts still work when playlist-read-private is missing, rather than blanking the section', async () => {
    stubRoutes({
      '/api/me': me(['playlist-read-private']),
      '/api/stats/context': contextPayload({
        playlistCoverage: { covered: 5, total: 10 },
        playlists: [playlistEntry({ contextType: 'playlist', contextId: 'pl-2', name: '', plays: 5 })],
      }),
      '/api/stats/taste': tastePayload(),
    })

    render(mountAt('/habits'))
    await waitForPage()

    // The section is not blanked: the denominator banner and the count still
    // render normally, exactly as they would with the scope granted.
    expect(
      await screen.findByText(
        "Based on the 50% of your listening Encore recorded live. No Spotify export records what you were playing from, so imported history cannot contribute.",
      ),
    ).toBeInTheDocument()
    expect(await findPlaylistChartCaption()).toHaveTextContent('led by Unknown playlist with 5')

    // The precise reason, and the reassurance that it does not touch the
    // counts, since those come from sync rather than from a playlists
    // request.
    expect(
      screen.getByText(
        "Playlist names aren't available without playlist-read-private, which this account has not granted. The counts above still work: they come from what Encore has synced, not from a request to your Spotify playlists.",
      ),
    ).toBeInTheDocument()
  })

  it('does not show the missing-scope note when the scope is granted', async () => {
    stubRoutes({
      '/api/me': me([]),
      '/api/stats/context': contextPayload({
        playlistCoverage: { covered: 5, total: 10 },
        playlists: [playlistEntry({ contextType: 'playlist', contextId: 'pl-3', name: '', plays: 5 })],
      }),
      '/api/stats/taste': tastePayload(),
    })

    render(mountAt('/habits'))
    await waitForPage()

    expect(await findPlaylistChartCaption()).toHaveTextContent('led by Unknown playlist with 5')
    expect(screen.queryByText(/playlist-read-private/)).not.toBeInTheDocument()
  })
})
