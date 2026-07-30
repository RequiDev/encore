/**
 * The library page's four states.
 *
 * Three of the four are more likely than the happy path on an upgraded
 * instance — a missing scope, a library that has never been enumerated, and
 * one that has been enumerated and is genuinely empty are all different
 * facts, and `syncedAt` being nullable exists precisely so the second and
 * third do not collapse into the same screen. These tests pin the exact
 * wording for each, and pin that a genuinely missing scope never falls back
 * to an empty-library message.
 */

import type { ReactElement } from 'react'
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryRouter } from 'react-router-dom'
import { render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { routes } from '../App'
import { createQueryClient } from '../lib/query'
import type { ArtistRef, LibraryStatsResponse, MeResponse, TrackRef } from '../lib/types'

function me(missingScopes: string[] = []): MeResponse {
  return {
    user: {
      id: 'cf0a1e6c-0000-4000-8000-000000000001',
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

function track(overrides: Partial<TrackRef> = {}): TrackRef {
  return {
    id: 'track-1',
    name: 'A Saved Track',
    durationMs: 200_000,
    explicit: false,
    album: {
      id: 'album-1',
      name: 'A Test Album',
      imageUrl: '',
      releaseDate: '2016-05-20',
      releasePrecision: 'day',
    },
    artists: [{ id: 'artist-1', name: 'A Test Artist', imageUrl: '' }],
    ...overrides,
  }
}

function artist(overrides: Partial<ArtistRef> = {}): ArtistRef {
  return { id: 'artist-2', name: 'A Followed Artist', imageUrl: '', ...overrides }
}

function libraryPayload(overrides: Partial<LibraryStatsResponse> = {}): LibraryStatsResponse {
  return {
    syncedAt: null,
    savedTracks: 0,
    savedAlbums: 0,
    followedArtists: 0,
    savedNeverPlayed: [],
    playedNeverSaved: [],
    dormantFollows: [],
    ...overrides,
  }
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
  await screen.findByRole('heading', { level: 1, name: 'Library' })
}

beforeEach(() => {
  vi.unstubAllGlobals()
})

describe('the library page', () => {
  it('says the library is not shared, and never asks the server for it, when a required scope is missing', async () => {
    stubRoutes({ '/api/me': me(['user-library-read']) })

    render(mountAt('/library'))
    await waitForPage()

    const heading = screen.getByText(/your spotify library isn't shared with encore/i)
    expect(heading).toBeInTheDocument()
    expect(screen.queryByText(/you have not saved anything/i)).not.toBeInTheDocument()

    // The reconsent banner also renders a "Reconnect Spotify" link for the
    // same missing scope, so the page's own must be found within its panel
    // rather than by name alone.
    const section = heading.closest('section')
    if (!section) throw new Error('the scope-gate heading is not inside a panel')
    const link = within(section).getByRole('link', { name: /reconnect spotify/i })
    expect(link).toHaveAttribute('href', '/api/auth/spotify/relink')

    // The one call made is /api/me; the page never asks for a library it
    // already knows has not been shared.
    const calledPaths = vi
      .mocked(fetch)
      .mock.calls.map(([input]) => new URL(String(input), 'http://encore.test').pathname)
    expect(calledPaths).not.toContain('/api/stats/library')
  })

  it('is also blocked by a missing user-follow-read alone', async () => {
    stubRoutes({ '/api/me': me(['user-follow-read']) })

    render(mountAt('/library'))
    await waitForPage()

    expect(
      screen.getByText(/your spotify library isn't shared with encore/i),
    ).toBeInTheDocument()
  })

  it('says Encore has not read the library yet when syncedAt is null, and renders no counts', async () => {
    stubRoutes({
      '/api/me': me([]),
      '/api/stats/library': libraryPayload({ syncedAt: null }),
    })

    render(mountAt('/library'))
    await waitForPage()

    expect(
      await screen.findByText(/encore has not read your spotify library yet/i),
    ).toBeInTheDocument()
    expect(
      screen.getByText(/it checks once a day; this page will fill in after the next run/i),
    ).toBeInTheDocument()

    // Not a zero-filled dashboard: no "Saved tracks" stat, no "0" anywhere
    // that would read as a measurement rather than as "not read yet".
    expect(screen.queryByText('Saved tracks')).not.toBeInTheDocument()
    expect(screen.queryByText(/last read from spotify/i)).not.toBeInTheDocument()
  })

  it('says nothing has been saved when synced and genuinely empty, distinct from never synced', async () => {
    const syncedAt = new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString()
    stubRoutes({
      '/api/me': me([]),
      '/api/stats/library': libraryPayload({ syncedAt }),
    })

    render(mountAt('/library'))
    await waitForPage()

    expect(
      await screen.findByText('You have not saved anything on Spotify'),
    ).toBeInTheDocument()
    expect(
      screen.queryByText(/encore has not read your spotify library yet/i),
    ).not.toBeInTheDocument()
  })

  it('says when it was last read, and shows the three lists with their own scoping', async () => {
    const syncedAt = new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString()
    stubRoutes({
      '/api/me': me([]),
      '/api/stats/library': libraryPayload({
        syncedAt,
        savedTracks: 120,
        savedAlbums: 14,
        followedArtists: 30,
        savedNeverPlayed: [
          { entity: track({ id: 'saved-1', name: 'A Saved Track' }), addedAt: '2020-01-01T00:00:00Z' },
        ],
        playedNeverSaved: [
          { entity: track({ id: 'played-1', name: 'A Played Track' }), plays: 12, msPlayed: 2_400_000 },
        ],
        dormantFollows: [
          { entity: artist({ id: 'dormant-1', name: 'A Dormant Artist' }), lastPlayedAt: null },
        ],
      }),
    })

    render(mountAt('/library'))
    await waitForPage()

    expect(await screen.findByText('Last read from Spotify 3 hours ago.')).toBeInTheDocument()

    const snapshot = (await screen.findByRole('heading', { name: 'Snapshot' })).closest('section')
    if (!snapshot) throw new Error('the Snapshot heading is not inside a panel')
    expect(within(snapshot).getByText('120')).toBeInTheDocument()
    expect(within(snapshot).getByText('14')).toBeInTheDocument()
    expect(within(snapshot).getByText('30')).toBeInTheDocument()

    const savedSection = (
      await screen.findByRole('heading', { name: 'Saved but never played' })
    ).closest('section')
    if (!savedSection) throw new Error('the "Saved but never played" heading is not inside a panel')
    expect(within(savedSection).getByText('A Saved Track')).toBeInTheDocument()
    expect(within(savedSection).getByText(/all time/i)).toBeInTheDocument()

    const playedSection = (
      await screen.findByRole('heading', { name: 'Played but never saved' })
    ).closest('section')
    if (!playedSection) throw new Error('the "Played but never saved" heading is not inside a panel')
    expect(within(playedSection).getByText('A Played Track')).toBeInTheDocument()
    // Range-scoped, unlike the panel above: the description names the range
    // rather than saying "all time".
    expect(within(playedSection).getByText(/last 30 days/i)).toBeInTheDocument()

    const dormantSection = (
      await screen.findByRole('heading', { name: 'Dormant follows' })
    ).closest('section')
    if (!dormantSection) throw new Error('the "Dormant follows" heading is not inside a panel')
    expect(within(dormantSection).getByText('A Dormant Artist')).toBeInTheDocument()
    expect(within(dormantSection).getByText('Never played')).toBeInTheDocument()
    expect(within(dormantSection).getByText(/last 30 days/i)).toBeInTheDocument()
  })
})
