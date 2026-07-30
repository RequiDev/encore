/**
 * The re-consent banner, mounted the way a person actually meets it: inside
 * the real shell, above whatever page they landed on.
 *
 * A refresh token minted before the scope set grew carries the old grant
 * forever, so `/api/me` reports the shortfall on `spotify.missingScopes` and
 * the shell is expected to say so — without blocking the page underneath it.
 * The behaviour worth pinning here is the dismissal: it has to survive a
 * reload for the same shortfall, and it has to let go the moment the
 * shortfall itself changes, so a later phase asking for one more scope is not
 * silently swallowed by today's click.
 */

import type { ReactElement } from 'react'
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryRouter } from 'react-router-dom'
import { fireEvent, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { routes } from '../App'
import { createQueryClient } from '../lib/query'
import type { MeResponse } from '../lib/types'

function meWithMissingScopes(missingScopes: string[]): MeResponse {
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

/** Waits for the shell to have actually loaded the session before asserting. */
async function waitForShell(): Promise<void> {
  await screen.findByRole('heading', { level: 1, name: 'Dashboard' })
}

function findDismissButton(): HTMLElement {
  return screen.getByRole('button', { name: /dismiss/i })
}

beforeEach(() => {
  vi.unstubAllGlobals()
  window.localStorage.clear()
})

describe('ReconsentBanner', () => {
  it('says nothing when the grant is current', async () => {
    stubFetch(200, meWithMissingScopes([]))

    render(mountAt('/'))
    await waitForShell()

    expect(screen.queryByRole('region', { name: /spotify permissions/i })).not.toBeInTheDocument()
  })

  it('explains each missing scope in plain language', async () => {
    stubFetch(200, meWithMissingScopes(['user-library-read']))

    render(mountAt('/'))
    await waitForShell()

    expect(screen.getByText(/saved but never played/i)).toBeInTheDocument()
    expect(screen.queryByText('user-library-read')).not.toBeInTheDocument()
  })

  it('promises no write access', async () => {
    stubFetch(200, meWithMissingScopes(['user-library-read']))

    render(mountAt('/'))
    await waitForShell()

    expect(
      screen.getByText(/none of these let encore change anything on your spotify account/i),
    ).toBeInTheDocument()
  })

  it('stays dismissed for the same set of scopes', async () => {
    stubFetch(200, meWithMissingScopes(['user-library-read']))
    const first = render(mountAt('/'))
    await waitForShell()

    fireEvent.click(findDismissButton())
    expect(screen.queryByRole('region', { name: /spotify permissions/i })).not.toBeInTheDocument()
    first.unmount()

    // A fresh mount with a fresh query client: what a reload looks like. Only
    // `localStorage` carries over, which is the point being tested.
    stubFetch(200, meWithMissingScopes(['user-library-read']))
    render(mountAt('/'))
    await waitForShell()

    expect(screen.queryByRole('region', { name: /spotify permissions/i })).not.toBeInTheDocument()
  })

  it('returns for a different set of scopes', async () => {
    stubFetch(200, meWithMissingScopes(['user-library-read']))
    const first = render(mountAt('/'))
    await waitForShell()

    fireEvent.click(findDismissButton())
    first.unmount()

    stubFetch(200, meWithMissingScopes(['user-top-read']))
    render(mountAt('/'))
    await waitForShell()

    expect(screen.getByRole('region', { name: /spotify permissions/i })).toBeInTheDocument()
    expect(screen.getByText(/compare spotify's own ranking to yours/i)).toBeInTheDocument()
  })
})
