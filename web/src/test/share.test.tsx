/**
 * The shared page, seen by somebody who has no account here.
 *
 * The property worth a test is the one that is easy to break by accident: the
 * route sits outside RequireAuth, so a signed-out visitor must get the page and
 * not the login screen. Wrapping it in the shell, or moving it under the auth
 * guard, would turn a working link into a sign-in prompt for everyone it was
 * ever sent to — and the owner would have no way to notice.
 */

import type { ReactElement } from 'react'
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryRouter } from 'react-router-dom'
import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { routes } from '../App'
import { createQueryClient } from '../lib/query'
import type { SharedStats } from '../lib/types'

const TOKEN = 'a'.repeat(43)

const SHARED: SharedStats = {
  label: 'My 2026',
  displayName: 'Someone',
  avatarUrl: '',
  timezone: 'UTC',
  rolling: false,
  rangeDays: 0,
  from: '2026-01-01T00:00:00Z',
  to: '2026-07-27T00:00:00Z',
  interval: 'month',
  summary: {
    listens: 1280,
    distinctTracks: 410,
    distinctArtists: 96,
    distinctAlbums: 150,
    msPlayed: 4_320_000_000,
    activeDays: 180,
    firstListenAt: '2026-01-02T09:00:00Z',
    lastListenAt: '2026-07-26T22:00:00Z',
  },
  tracks: {
    items: [
      {
        entity: {
          id: 'trk1',
          name: 'Weightless',
          durationMs: 214000,
          explicit: false,
          album: null,
          artists: [{ id: 'art1', name: 'Marconi Union', imageUrl: '' }],
        },
        plays: 42,
        msPlayed: 8_000_000,
        rank: 1,
        previousRank: null,
      },
    ],
    total: 1,
  },
  artists: {
    items: [
      {
        entity: { id: 'art1', name: 'Marconi Union', imageUrl: '' },
        plays: 96,
        msPlayed: 12_000_000,
        rank: 1,
        previousRank: null,
      },
    ],
    total: 1,
  },
  albums: { items: [], total: 0 },
  genres: {
    genres: [
      { genre: 'dream pop', plays: 120, msPlayed: 20_000_000 },
      { genre: 'ambient', plays: 80, msPlayed: 15_000_000 },
    ],
    total: 2,
    coverage: { covered: 1000, total: 1280 },
  },
  taste: {
    obscurity: { value: 57, covered: 1200, total: 1280 },
    releaseLag: { value: 8.4, covered: 1100, total: 1280 },
  },
  timeline: [],
  hours: [],
  weekdays: [],
}

function stubShare(status: number, body: unknown): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const path = new URL(
        typeof input === 'string' ? input : input.toString(),
        'http://encore.test',
      ).pathname
      if (path === '/api/me') {
        // No session: exactly what a stranger's browser gets.
        return new Response(
          JSON.stringify({ error: { code: 'unauthenticated', message: 'No session.' } }),
          { status: 401, headers: { 'content-type': 'application/json' } },
        )
      }
      return new Response(JSON.stringify(body), {
        status,
        headers: { 'content-type': 'application/json' },
      })
    }),
  )
}

function mountShare(): ReactElement {
  const router = createMemoryRouter(routes, { initialEntries: [`/share/${TOKEN}`] })
  return (
    <QueryClientProvider client={createQueryClient()}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}

beforeEach(() => {
  vi.unstubAllGlobals()
})

describe('a shared link', () => {
  it('renders for a visitor with no session', async () => {
    stubShare(200, SHARED)

    render(mountShare())

    expect(await screen.findByRole('heading', { level: 1, name: 'My 2026' })).toBeInTheDocument()
    expect(screen.getByText(/Someone/)).toBeInTheDocument()
    expect(screen.getByText('1,280')).toBeInTheDocument()
    // Appears twice: as the top artist, and as the track's credit.
    expect(screen.getAllByText('Marconi Union').length).toBeGreaterThan(0)

    // Genres and taste are aggregate taste, the same data class as the top
    // lists, so a share carries them — each with its own coverage sentence.
    expect(screen.getByText('dream pop')).toBeInTheDocument()
    expect(screen.getByText(/Genres are known for 78.1% of this listening/)).toBeInTheDocument()
    expect(screen.getByText('57')).toBeInTheDocument()
    expect(screen.getByText('8.4')).toBeInTheDocument()

    // Not the login screen, and no application navigation: a visitor is not a
    // user of this instance and is shown nothing that suggests otherwise.
    expect(screen.queryByRole('link', { name: /continue with spotify/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('navigation', { name: 'Main' })).not.toBeInTheDocument()
  })

  it('says plainly when a link has been revoked', async () => {
    stubShare(404, { error: { code: 'not_found', message: 'That link does not exist.' } })

    render(mountShare())

    expect(await screen.findByText(/this link does not work/i)).toBeInTheDocument()
    // The visitor is told it is not their problem, rather than being offered a
    // retry that cannot succeed.
    expect(screen.getByText(/nothing to fix at your end/i)).toBeInTheDocument()
  })
})
