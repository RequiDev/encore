/**
 * Album completion, on the album page and in the dashboard's own aggregate.
 *
 * The two figures share a name but not a denominator: the album page's own
 * completion is all time, while the dashboard's "albums completed" count is
 * scoped to the range picker like everything else there. Nothing in either
 * response says so — only the copy does — so these tests pin the exact
 * wording rather than just the numbers, and pin the unresolved-track-count
 * state to a message rather than a ratio it cannot compute.
 */

import type { ReactElement } from 'react'
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryRouter } from 'react-router-dom'
import { render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { routes } from '../App'
import { createQueryClient } from '../lib/query'
import type { AlbumDetail, MeResponse, StatsExtras, Summary } from '../lib/types'

const ME: MeResponse = {
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
    missingScopes: [],
  },
  csrfToken: 'not-a-real-token',
  listening: {
    firstListenAt: '2019-03-04T12:00:00.000Z',
    lastListenAt: '2026-07-26T09:00:00.000Z',
  },
  instance: { registrationsEnabled: false, version: '1.0.0' },
}

function albumPayload(completion: AlbumDetail['completion']): AlbumDetail {
  return {
    album: {
      id: 'album-1',
      name: 'A Test Record',
      imageUrl: '',
      releaseDate: '2016-05-20',
      releasePrecision: 'day',
      albumType: 'album',
      totalTracks: 12,
      artists: [{ id: 'artist-1', name: 'A Test Artist', imageUrl: '' }],
    },
    stats: {
      plays: 20,
      msPlayed: 1_000_000,
      firstListenAt: '2026-06-01T00:00:00Z',
      lastListenAt: '2026-06-20T00:00:00Z',
      discoveredAt: '2020-01-01T00:00:00Z',
      lastPlayedAt: '2026-06-20T00:00:00Z',
      timeline: [],
    },
    topTracks: [],
    completion,
  }
}

function summary(overrides: Partial<Summary> = {}): Summary {
  return {
    listens: 500,
    distinctTracks: 120,
    distinctArtists: 40,
    distinctAlbums: 30,
    msPlayed: 12_000_000,
    activeDays: 20,
    firstListenAt: '2026-06-01T00:00:00Z',
    lastListenAt: '2026-06-20T00:00:00Z',
    ...overrides,
  }
}

function extras(overrides: Partial<StatsExtras> = {}): StatsExtras {
  return {
    differentArtists: 40,
    averageAlbumReleaseYear: 2015,
    averageArtistsPerTrack: 1.2,
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

/** The "Heard" panel, found the way a person finds it: by its heading. */
async function heardPanel(): Promise<HTMLElement> {
  const heading = await screen.findByRole('heading', { name: 'Heard' })
  const section = heading.closest('section')
  if (!section) throw new Error('the Heard heading is not inside a panel')
  return section
}

/** The "Also worth knowing" panel on the dashboard, the same way. */
async function extrasPanel(): Promise<HTMLElement> {
  const heading = await screen.findByRole('heading', { name: 'Also worth knowing' })
  const section = heading.closest('section')
  if (!section) throw new Error('the "Also worth knowing" heading is not inside a panel')
  return section
}

beforeEach(() => {
  vi.unstubAllGlobals()
})

describe('the album page completion figure', () => {
  it('says the track count is not known yet, and links to Settings, rather than a ratio', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 0, total: 0, known: false }),
    })

    render(mountAt('/albums/album-1'))
    const section = await heardPanel()

    expect(within(section).getByText(/track count not known yet/i)).toBeInTheDocument()
    expect(within(section).queryByText(/%/)).not.toBeInTheDocument()
    expect(within(section).queryByText(/\d+ of \d+/)).not.toBeInTheDocument()

    const link = within(section).getByRole('link', { name: 'Settings' })
    expect(link).toHaveAttribute('href', '/settings')
  })

  it('says every track was heard rather than a bare ratio, and still carries "all time"', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 12, total: 12, known: true }),
    })

    render(mountAt('/albums/album-1'))
    const section = await heardPanel()

    expect(within(section).getByText('Every track')).toBeInTheDocument()
    expect(within(section).queryByText('12 of 12')).not.toBeInTheDocument()
    expect(within(section).getByText('all time')).toBeInTheDocument()
  })

  it('gives an in-progress ratio the same all-time qualifier as first and last listen', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 9, total: 12, known: true }),
    })

    render(mountAt('/albums/album-1'))
    const section = await heardPanel()

    expect(within(section).getByText('9 of 12')).toBeInTheDocument()
    expect(within(section).getByText('tracks')).toBeInTheDocument()
    expect(within(section).getByText('all time')).toBeInTheDocument()
  })

  it('says "track" rather than "tracks" for a one-track album, unplayed', async () => {
    // Regression: the suffix used to be hard-coded to the plural, so a single
    // unplayed track read "0 of 1 tracks" directly above two other panels on
    // the same page that both get this right — the completion figure was the
    // odd one out on its own screen.
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 0, total: 1, known: true }),
    })

    render(mountAt('/albums/album-1'))
    const section = await heardPanel()

    expect(within(section).getByText('0 of 1')).toBeInTheDocument()
    expect(within(section).getByText('track')).toBeInTheDocument()
    expect(within(section).queryByText('tracks')).not.toBeInTheDocument()
  })
})

describe('the dashboard\'s "albums completed" aggregate', () => {
  it('names its own denominator instead of shipping a bare count', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/stats/summary': summary(),
      '/api/stats/extras': extras({ albumsCompleted: { complete: 12, albums: 87 } }),
    })

    render(mountAt('/'))
    const section = await extrasPanel()

    expect(
      await within(section).findByText(
        'Heard every track on 12 of the 87 albums with a known track count you played in this range.',
      ),
    ).toBeInTheDocument()
  })

  it('says so plainly when no album in range has a known track count', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/stats/summary': summary(),
      '/api/stats/extras': extras(),
    })

    render(mountAt('/'))
    const section = await extrasPanel()

    expect(
      await within(section).findByText(
        'No albums with a known track count were played in this range.',
      ),
    ).toBeInTheDocument()
  })
})
