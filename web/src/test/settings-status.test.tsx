/**
 * The metadata panel on the Settings page.
 *
 * This is the one place in the interface that answers "why are the artists
 * blank?", and the answer it has to give is unintuitive: nothing is broken,
 * nothing needs restarting, and the listening figures are already complete. The
 * tests below pin the three states apart, because a panel that reported a
 * rate-limited instance as merely "filling in" would send someone off restarting
 * containers — which is the one action that makes the wait longer.
 */

import type { ReactElement } from 'react'
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryRouter } from 'react-router-dom'
import { render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { routes } from '../App'
import { createQueryClient } from '../lib/query'
import type { EntityProgress, MeResponse, StatusResponse } from '../lib/types'

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
  },
  csrfToken: 'not-a-real-token',
  listening: {
    firstListenAt: '2019-03-04T12:00:00.000Z',
    lastListenAt: '2026-07-26T09:00:00.000Z',
  },
  instance: { registrationsEnabled: false, version: '1.0.0' },
}

function entity(over: Partial<EntityProgress> = {}): EntityProgress {
  return {
    total: 0,
    resolved: 0,
    pending: 0,
    failed: 0,
    unavailable: 0,
    named: 0,
    local: 0,
    ...over,
  }
}

function status(over: Partial<StatusResponse['metadata']> = {}): StatusResponse {
  return {
    catalogue: {
      // Deliberately the shape a real import leaves behind: every track named
      // from the export itself, but almost none of them described by Spotify
      // yet, and the artists still anonymous.
      tracks: entity({ total: 16505, resolved: 50, pending: 16455, named: 16505 }),
      artists: entity({ total: 29, pending: 29 }),
      albums: entity({ total: 40, resolved: 40, named: 40 }),
      aliasesTotal: 0,
      aliasesPending: 0,
    },
    metadata: {
      outstanding: 16484,
      complete: false,
      paused: false,
      pausedUntil: null,
      fallbackConfigured: false,
      ...over,
    },
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

function mountSettings(): ReactElement {
  const router = createMemoryRouter(routes, { initialEntries: ['/settings'] })
  return (
    <QueryClientProvider client={createQueryClient()}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}

/** The metadata panel, found the way a person finds it: by its heading. */
async function panel(): Promise<HTMLElement> {
  const heading = await screen.findByRole('heading', { name: /music metadata/i })
  const section = heading.closest('section')
  if (!section) throw new Error('the metadata heading is not inside a panel')
  return section
}

beforeEach(() => {
  vi.unstubAllGlobals()
})

describe('the metadata panel', () => {
  it('reports what is named and what is still queued', async () => {
    stubRoutes({ '/api/me': ME, '/api/blacklist': [], '/api/status': status() })

    render(mountSettings())
    const section = await panel()

    expect(await within(section).findByText(/filling in/i)).toBeInTheDocument()

    // The bar measures names, not full resolution: every track is readable even
    // though Spotify has described almost none of them.
    const tracks = within(section).getByRole('progressbar', { name: /tracks named/i })
    expect(tracks).toHaveAttribute('aria-valuenow', '100')
    const artists = within(section).getByRole('progressbar', { name: /artists named/i })
    expect(artists).toHaveAttribute('aria-valuenow', '0')

    expect(within(section).getByText(/16,455 waiting on Spotify/)).toBeInTheDocument()
    expect(within(section).getByText(/still to fetch/i)).toBeInTheDocument()
  })

  it('says so plainly when Spotify has rate limited the instance', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/blacklist': [],
      '/api/status': status({ paused: true, pausedUntil: '2026-07-28T04:00:00Z' }),
    })

    render(mountSettings())
    const section = await panel()

    expect(await within(section).findByText(/rate limited/i)).toBeInTheDocument()
    // The two facts that stop someone acting on a problem that is not theirs.
    expect(within(section).getByText(/resumes by itself/i)).toBeInTheDocument()
    expect(within(section).getByText(/listening data is unaffected/i)).toBeInTheDocument()
    expect(within(section).getByText(/Restarting\s+does\s+not\s+help/i)).toBeInTheDocument()
  })

  it('says enrichment continues when a fallback source is configured', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/blacklist': [],
      '/api/status': status({
        paused: true,
        pausedUntil: '2026-07-28T04:00:00Z',
        fallbackConfigured: true,
      }),
    })

    render(mountSettings())
    const section = await panel()

    expect(await within(section).findByText(/rate limited/i)).toBeInTheDocument()
    // With a second source the wait is not a stoppage, and saying otherwise
    // would send someone looking for a problem that is already handled.
    expect(within(section).getByText(/reading from the fallback source/i)).toBeInTheDocument()
    expect(within(section).queryByText(/resumes by itself/i)).not.toBeInTheDocument()
  })

  it('stops asking for attention once everything has been fetched', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/blacklist': [],
      '/api/status': {
        catalogue: {
          tracks: entity({ total: 12, resolved: 12, named: 12 }),
          artists: entity({ total: 5, resolved: 5, named: 5 }),
          albums: entity({ total: 7, resolved: 7, named: 7 }),
          aliasesTotal: 0,
          aliasesPending: 0,
        },
        metadata: {
          outstanding: 0,
          complete: true,
          paused: false,
          pausedUntil: null,
          fallbackConfigured: false,
        },
      } satisfies StatusResponse,
    })

    render(mountSettings())
    const section = await panel()

    expect(await within(section).findByText(/complete/i)).toBeInTheDocument()
    expect(within(section).queryByText(/waiting on Spotify/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/rate limited/i)).not.toBeInTheDocument()
  })
})
