/**
 * The top-diff page's states.
 *
 * Mirrors `library.test.tsx`'s approach to the same shape of problem: a
 * missing scope, a `(kind, range)` set that has never been captured, and a
 * genuinely empty comparison are three different facts, and these tests pin
 * the exact wording for each so the second and third can never collapse into
 * the same screen. They also pin that the "Spotify's ranking is opaque"
 * disclaimer is always on screen once the scope is granted — that sentence
 * is the whole reason the feature is defensible, so it must not depend on
 * data having loaded.
 */

import type { ReactElement } from 'react'
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryRouter } from 'react-router-dom'
import { render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { routes } from '../App'
import { createQueryClient } from '../lib/query'
import type { ArtistRef, MeResponse, TopDiffResponse } from '../lib/types'

function me(missingScopes: string[] = []): MeResponse {
  return {
    user: {
      id: 'cf0a1e6c-0000-4000-8000-000000000002',
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

function artist(overrides: Partial<ArtistRef> = {}): ArtistRef {
  return { id: 'artist-1', name: 'A Compared Artist', imageUrl: '', ...overrides }
}

function topDiffPayload(
  overrides: Partial<TopDiffResponse<ArtistRef>> = {},
): TopDiffResponse<ArtistRef> {
  return { capturedAt: null, timeRange: 'short_term', entries: [], ...overrides }
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
  await screen.findByRole('heading', { level: 1, name: 'Top diff' })
}

beforeEach(() => {
  vi.unstubAllGlobals()
})

describe('the top-diff page', () => {
  it('says Spotify is opaque before any data has loaded, and never asks the server for it, when the scope is missing', async () => {
    stubRoutes({ '/api/me': me(['user-top-read']) })

    render(mountAt('/top-diff'))
    await waitForPage()

    // The disclaimer is not conditional on data: it must be visible even in
    // the scope-blocked state, because it explains the whole feature rather
    // than any one result.
    expect(screen.getByText(/spotify calls this "calculated affinity"/i)).toBeInTheDocument()

    const heading = screen.getByText(/spotify's own top ranking isn't shared with encore/i)
    expect(heading).toBeInTheDocument()

    const section = heading.closest('section')
    if (!section) throw new Error('the scope-gate heading is not inside a panel')
    const link = within(section).getByRole('link', { name: /reconnect spotify/i })
    expect(link).toHaveAttribute('href', '/api/auth/spotify/relink')

    const calledPaths = vi
      .mocked(fetch)
      .mock.calls.map(([input]) => new URL(String(input), 'http://encore.test').pathname)
    expect(calledPaths).not.toContain('/api/stats/top-diff')
  })

  it('says Encore has not captured the ranking yet when capturedAt is null, and renders no table', async () => {
    stubRoutes({
      '/api/me': me([]),
      '/api/stats/top-diff': topDiffPayload({ capturedAt: null }),
    })

    render(mountAt('/top-diff'))
    await waitForPage()

    expect(
      await screen.findByText(/encore has not captured spotify's top artists ranking yet/i),
    ).toBeInTheDocument()
    expect(
      screen.getByText(/checked once a day, alongside your library sync/i),
    ).toBeInTheDocument()
    expect(screen.queryByText('Captured')).not.toBeInTheDocument()
  })

  it('says Spotify reported nothing for this window when captured but empty, distinct from never captured', async () => {
    const capturedAt = new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString()
    stubRoutes({
      '/api/me': me([]),
      '/api/stats/top-diff': topDiffPayload({ capturedAt, entries: [] }),
    })

    render(mountAt('/top-diff'))
    await waitForPage()

    expect(
      await screen.findByText('Spotify reported no top artists for this window'),
    ).toBeInTheDocument()
    expect(
      screen.queryByText(/encore has not captured spotify's top artists ranking yet/i),
    ).not.toBeInTheDocument()
  })

  it('renders both ranks side by side, and a dash for the side that is absent', async () => {
    const capturedAt = new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString()
    stubRoutes({
      '/api/me': me([]),
      '/api/stats/top-diff': topDiffPayload({
        capturedAt,
        entries: [
          { entity: artist({ id: 'both', name: 'On Both Sides' }), spotifyRank: 2, encoreRank: 1, plays: 12 },
          {
            entity: artist({ id: 'spotify-only', name: 'Only Spotify Ranks This' }),
            spotifyRank: 1,
            encoreRank: null,
            plays: 0,
          },
        ],
      }),
    })

    render(mountAt('/top-diff'))
    await waitForPage()

    expect(
      await screen.findByText(/captured 3 hours ago — refreshed once a day/i),
    ).toBeInTheDocument()

    const bothRow = (await screen.findByText('On Both Sides')).closest('tr')
    if (!bothRow) throw new Error('row not found')
    expect(within(bothRow).getByText('2')).toBeInTheDocument()
    expect(within(bothRow).getByText('1')).toBeInTheDocument()
    expect(within(bothRow).getByText('12')).toBeInTheDocument()

    const spotifyOnlyRow = (await screen.findByText('Only Spotify Ranks This')).closest('tr')
    if (!spotifyOnlyRow) throw new Error('row not found')
    expect(within(spotifyOnlyRow).getByTitle(/outside encore's ranking/i)).toBeInTheDocument()
  })
})
