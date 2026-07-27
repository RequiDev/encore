/**
 * The shell's wiring, end to end but offline.
 *
 * `fetch` is stubbed, so this exercises the pieces that are easy to break
 * silently and impossible to see in a type check: that a signed-out visitor
 * reaches the login screen instead of a broken page, that a signed-in one gets
 * the navigation and their page, and that the accessibility floor — a skip link
 * first, exactly one h1 — actually holds once everything is composed.
 *
 * The real route tree is mounted on a memory router with a fresh query client,
 * so no test inherits another's cached session or history position.
 */

import type { ReactElement } from 'react'
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryRouter } from 'react-router-dom'
import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { routes } from '../App'
import { createQueryClient } from '../lib/query'
import type { MeResponse } from '../lib/types'

const ME: MeResponse = {
  user: {
    id: 'cf0a1e6c-0000-4000-8000-000000000001',
    spotifyUserId: 'someone',
    displayName: 'Someone',
    email: 'someone@example.com',
    avatarUrl: '',
    role: 'admin',
    isActive: true,
    timezone: 'Europe/Berlin',
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
  listening: { firstListenAt: '2019-03-04T12:00:00.000Z', lastListenAt: '2026-07-26T09:00:00.000Z' },
  instance: { registrationsEnabled: false, version: '1.0.0' },
}

function stubFetch(status: number, body: unknown): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async () =>
        new Response(JSON.stringify(body), {
          status,
          headers: { 'content-type': 'application/json' },
        }),
    ),
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

beforeEach(() => {
  vi.unstubAllGlobals()
})

describe('the application shell', () => {
  it('sends a signed-out visitor to the login screen', async () => {
    stubFetch(401, { error: { code: 'unauthenticated', message: 'No session.' } })

    render(mountAt('/'))

    expect(
      await screen.findByRole('heading', { level: 1, name: /listening history, kept by you/i }),
    ).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /continue with spotify/i })).toHaveAttribute(
      'href',
      expect.stringContaining('/api/auth/spotify/login'),
    )
  })

  it('gives a signed-in visitor the shell, its navigation and one h1', async () => {
    stubFetch(200, ME)

    render(mountAt('/'))

    expect(await screen.findByRole('heading', { level: 1, name: 'Dashboard' })).toBeInTheDocument()
    expect(screen.getAllByRole('heading', { level: 1 })).toHaveLength(1)

    // The skip link has to be the first thing a keyboard reaches.
    const skip = screen.getByRole('link', { name: /skip to content/i })
    expect(skip).toHaveAttribute('href', '#main')
    expect(document.body.querySelectorAll('a, button')[0]).toBe(skip)

    expect(screen.getByRole('navigation', { name: 'Main' })).toBeInTheDocument()
    expect(screen.getByRole('main')).toHaveAttribute('id', 'main')
  })

  it('shows an unmapped address as not found rather than a blank page', async () => {
    stubFetch(200, ME)

    render(mountAt('/nowhere-at-all'))

    expect(
      await screen.findByRole('heading', { level: 1, name: /page not found/i }),
    ).toBeInTheDocument()
  })

  it('keeps an administration route away from a plain user', async () => {
    stubFetch(200, { ...ME, user: { ...ME.user, role: 'user' } })

    render(mountAt('/settings/admin'))

    expect(await screen.findByRole('heading', { level: 1, name: 'Settings' })).toBeInTheDocument()
  })
})
