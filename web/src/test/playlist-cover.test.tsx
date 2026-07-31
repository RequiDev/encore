/**
 * The playlist row's rename control and its four cover states.
 *
 * Nothing in this project has ever been opened in a browser, and a rename is a
 * write to somebody's real Spotify account. These tests are the only thing
 * standing between a green suite and a row that offers a retry button for a
 * missing permission, or that says "renamed" for a request that got no answer.
 */

import type { ReactElement } from 'react'
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryRouter } from 'react-router-dom'
import { fireEvent, render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { routes } from '../App'
import { createQueryClient } from '../lib/query'
import { coverLine } from '../pages/Settings'
import type { EntityProgress, MeResponse, Playlist, PlaylistCover, StatusResponse } from '../lib/types'

const PLAYLIST_NAME = 'Heavy rotation'

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
    scopes: ['user-read-recently-played', 'playlist-modify-private'],
    missingScopes: [],
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
      ...over,
    },
  }
}

function cover(over: Partial<PlaylistCover> = {}): PlaylistCover {
  return {
    state: 'none',
    kind: '',
    covered: 0,
    total: 4,
    reason: '',
    at: null,
    ...over,
  }
}

function playlist(over: Partial<Playlist> = {}): Playlist {
  return {
    id: 'pl-1',
    name: PLAYLIST_NAME,
    spotifyId: 'sp1',
    spotifyUrl: 'https://open.spotify.com/playlist/sp1',
    mode: 'top',
    sort: 'plays',
    limit: 100,
    minPlays: 10,
    from: null,
    to: null,
    trackCount: 42,
    builtAt: '2026-07-20T10:00:00Z',
    cover: cover(),
    createdAt: '2026-07-01T00:00:00Z',
    ...over,
  }
}

/** A non-200 stubbed response, for the one path that must answer something else. */
interface StubResponse {
  status: number
  body: unknown
}

function respond(status: number, body: unknown): StubResponse {
  return { status, body }
}

function isStubResponse(value: unknown): value is StubResponse {
  return typeof value === 'object' && value !== null && 'status' in value && 'body' in value
}

/**
 * Answers each path with its own body, so one page can be given a whole API.
 * A plain value answers 200, exactly as in settings-status.test.tsx; wrapping
 * one in `respond()` is the one addition this file needs, for the rename
 * refusal below.
 */
function stubRoutes(bodies: Record<string, unknown>): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString()
      const path = new URL(url, 'http://encore.test').pathname
      const entry = bodies[path]
      if (entry === undefined) {
        return new Response(JSON.stringify({ error: { code: 'not_found', message: 'No.' } }), {
          status: 404,
          headers: { 'content-type': 'application/json' },
        })
      }
      const { status: code, body } = isStubResponse(entry) ? entry : { status: 200, body: entry }
      return new Response(JSON.stringify(body), {
        status: code,
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

/** The playlists panel, found the way a person finds it: by its heading. */
async function playlistPanel(): Promise<HTMLElement> {
  const heading = await screen.findByRole('heading', { name: /playlists/i })
  const section = heading.closest('section')
  if (!section) throw new Error('the playlists heading is not inside a panel')
  return section
}

beforeEach(() => {
  vi.unstubAllGlobals()
})

describe('coverLine', () => {
  // Fails when: the cover line for a full mosaic changes wording, or `4` starts
  // being interpolated from cover.total and the total drifts.
  it('says how many covers a full mosaic used', () => {
    expect(coverLine(cover({ state: 'ready', kind: 'mosaic', covered: 4 }))).toBe(
      'Cover built from 4 of 4 album covers.',
    )
  })

  // Fails when: the partial-mosaic branch is merged into the full one, which
  // would claim four covers were used when three were.
  it('says when part of the mosaic is pattern', () => {
    expect(coverLine(cover({ state: 'ready', kind: 'mosaic', covered: 3 }))).toBe(
      'Cover built from 3 of 4 album covers; the rest is a generated pattern.',
    )
  })

  // Fails when: a pattern cover is described as a mosaic.
  it('says a pattern cover is a pattern', () => {
    expect(coverLine(cover({ state: 'ready', kind: 'pattern', covered: 0 }))).toBe(
      'Cover is a generated pattern — Encore does not have artwork for these tracks yet.',
    )
  })

  // Fails when: a failure and a missing permission share a sentence.
  it('separates a failure from a missing permission', () => {
    expect(coverLine(cover({ state: 'failed', reason: 'Spotify would not accept the cover.' }))).toBe(
      "Encore's last attempt to set a cover did not finish. Spotify would not accept the cover.",
    )
    expect(coverLine(cover({ state: 'unauthorised' }))).toBe(
      'Encore does not have permission to set a cover for this playlist.',
    )
  })

  // Fails when: either sentence claims the account has no cover. SetCover
  // overwrites the whole cover block, so both `failed` and `unauthorised` can
  // follow a *replacement* attempt made against a playlist that already had a
  // mosaic — and "cover not generated" would be a false claim about that
  // playlist's actual artwork in that case.
  it('never claims the account has no cover, only that the last attempt did not finish', () => {
    expect(coverLine(cover({ state: 'failed', reason: 'Spotify would not accept the cover.' }))).not.toMatch(
      /not generated/i,
    )
    expect(coverLine(cover({ state: 'unauthorised' }))).not.toMatch(/not generated/i)
  })
})

describe('the playlist row', () => {
  // Fails when: the cover line for a full mosaic changes wording, or `4` starts
  // being interpolated from cover.total and the total drifts.
  it('says how many album covers a full mosaic used', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/blacklist': [],
      '/api/status': status(),
      '/api/playlists': [playlist({ cover: cover({ state: 'ready', kind: 'mosaic', covered: 4 }) })],
    })
    render(mountSettings())
    const section = await playlistPanel()
    expect(
      await within(section).findByText('Cover built from 4 of 4 album covers.'),
    ).toBeInTheDocument()
  })

  // Fails when: the partial-mosaic branch is merged into the full one, which
  // would claim four covers were used when three were.
  it('says when part of the mosaic is pattern', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/blacklist': [],
      '/api/status': status(),
      '/api/playlists': [playlist({ cover: cover({ state: 'ready', kind: 'mosaic', covered: 3 }) })],
    })
    render(mountSettings())
    const section = await playlistPanel()
    expect(
      await within(section).findByText(
        'Cover built from 3 of 4 album covers; the rest is a generated pattern.',
      ),
    ).toBeInTheDocument()
  })

  // Fails when: a pattern cover is described as a mosaic, which would tell
  // somebody Encore found artwork it did not find.
  it('says a pattern cover is a pattern, and why', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/blacklist': [],
      '/api/status': status(),
      '/api/playlists': [playlist({ cover: cover({ state: 'ready', kind: 'pattern', covered: 0 }) })],
    })
    render(mountSettings())
    const section = await playlistPanel()
    expect(
      await within(section).findByText(
        'Cover is a generated pattern — Encore does not have artwork for these tracks yet.',
      ),
    ).toBeInTheDocument()
  })

  // Fails when: the failed and unauthorised states share a branch — the retry
  // button then appears for a missing permission, which is the exact defect the
  // two states exist to prevent.
  it('offers a retry for a failure and a consent link for a missing permission', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/blacklist': [],
      '/api/status': status(),
      '/api/playlists': [
        playlist({
          id: 'pl-failed',
          name: 'Failed cover playlist',
          cover: cover({ state: 'failed', reason: 'Spotify would not accept the cover.' }),
        }),
        playlist({
          id: 'pl-unauth',
          name: 'Unauthorised cover playlist',
          cover: cover({ state: 'unauthorised' }),
        }),
      ],
    })
    render(mountSettings())
    const section = await playlistPanel()

    expect(
      await within(section).findByText(
        "Encore's last attempt to set a cover did not finish. Spotify would not accept the cover.",
      ),
    ).toBeInTheDocument()
    expect(within(section).getByRole('button', { name: /try again/i })).toBeInTheDocument()

    expect(
      within(section).getByText(
        'Encore does not have permission to set a cover for this playlist.',
      ),
    ).toBeInTheDocument()
    const consent = within(section).getByRole('link', { name: 'Allow Encore to set covers' })
    expect(consent).toHaveAttribute('href', '/api/auth/spotify/playlists')

    // And the two must not be interchangeable.
    const rows = within(section).getAllByRole('listitem')
    expect(rows).toHaveLength(2)
    expect(within(rows[1]!).queryByRole('button', { name: /try again/i })).not.toBeInTheDocument()
  })

  // Fails when: the "Building the cover…" status is bare JSXText rather than
  // a string literal in braces. JSX text children are taken verbatim — escape
  // sequences are not processed — so a Unicode escape sitting outside braces
  // renders as a literal backslash followed by "u2026" for the whole upload,
  // rather than the ellipsis the sibling `Renaming…` status (a string
  // literal in braces) actually shows. Nothing before this test rendered this
  // branch at all.
  it('shows "Building the cover…" — not a literal backslash escape — while a cover uploads', async () => {
    let resolveCover: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => {
      resolveCover = resolve
    })
    // Flips once the upload resolves, so the refetch that invalidateQueries
    // triggers on success reflects the new cover — the same sequence the real
    // handler produces, rather than a GET stub frozen on the pre-upload row.
    let coverReady = false

    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = typeof input === 'string' ? input : input.toString()
        const path = new URL(url, 'http://encore.test').pathname
        const method = (init?.method ?? 'GET').toUpperCase()
        const json = (code: number, body: unknown) =>
          new Response(JSON.stringify(body), {
            status: code,
            headers: { 'content-type': 'application/json' },
          })

        if (path === '/api/me') return json(200, ME)
        if (path === '/api/blacklist') return json(200, [])
        if (path === '/api/status') return json(200, status())
        if (path === '/api/playlists' && method === 'GET') {
          return json(200, [
            playlist({
              cover: coverReady ? cover({ state: 'ready', kind: 'mosaic', covered: 4 }) : cover(),
            }),
          ])
        }
        if (path === '/api/playlists/pl-1/cover' && method === 'POST') return pending
        return json(404, { error: { code: 'not_found', message: 'No.' } })
      }),
    )

    render(mountSettings())
    const section = await playlistPanel()

    fireEvent.click(
      await within(section).findByRole('button', { name: `Add cover for ${PLAYLIST_NAME}` }),
    )

    expect(await within(section).findByText('Building the cover…')).toBeInTheDocument()
    // The literal defect: a backslash-u escape rendered as visible text
    // rather than being parsed into the ellipsis it names.
    expect(within(section).queryByText(/u2026/)).not.toBeInTheDocument()

    coverReady = true
    resolveCover(
      new Response(
        JSON.stringify(playlist({ cover: cover({ state: 'ready', kind: 'mosaic', covered: 4 }) })),
        { status: 200, headers: { 'content-type': 'application/json' } },
      ),
    )

    expect(await within(section).findByText('Cover built from 4 of 4 album covers.')).toBeInTheDocument()
  })

  // Fails when: rebuildCover has no isError rendering. docs/api.md's "always
  // 200" only holds once handlePlaylistCover is reached — a request that
  // fails earlier (an expired session, an id Spotify no longer has) is a real
  // request failure, and silently doing nothing to the row would leave
  // somebody pressing a button that visibly does nothing.
  it('surfaces an error when the cover request itself fails, not just a cover outcome', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/blacklist': [],
      '/api/status': status(),
      '/api/playlists': [playlist()],
      '/api/playlists/pl-1/cover': respond(401, {
        error: { code: 'unauthenticated', message: 'Your session has expired. Sign in again.' },
      }),
    })
    render(mountSettings())
    const section = await playlistPanel()

    fireEvent.click(
      await within(section).findByRole('button', { name: `Add cover for ${PLAYLIST_NAME}` }),
    )

    expect(await within(section).findByRole('alert')).toHaveTextContent(/session has expired/i)
  })

  // Fails when: the rename hint is dropped. It is the one sentence telling
  // somebody this control writes to their Spotify account rather than to a
  // label inside Encore.
  it('says a rename reaches Spotify before it is confirmed', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/blacklist': [],
      '/api/status': status(),
      '/api/playlists': [playlist()],
    })
    render(mountSettings())
    const section = await playlistPanel()

    fireEvent.click(
      await within(section).findByRole('button', { name: `Rename ${PLAYLIST_NAME}` }),
    )

    expect(
      await within(section).findByText('This renames it in your Spotify account too.'),
    ).toBeInTheDocument()
  })

  // Fails when: the client adds its own error fallback over the server's
  // sentence. The server's four rename branches each say what is true of the
  // playlist afterwards, and "Something went wrong" replaces all four with
  // nothing.
  it('shows the server’s own sentence when a rename is refused', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/blacklist': [],
      '/api/status': status(),
      '/api/playlists': [playlist()],
      '/api/playlists/pl-1': respond(409, {
        error: {
          code: 'conflict',
          message:
            'Encore did not get an answer from Spotify, so it cannot tell whether the rename ' +
            'went through. Open the playlist in Spotify to check — renaming it again is safe ' +
            'either way.',
        },
      }),
    })
    render(mountSettings())
    const section = await playlistPanel()

    fireEvent.click(
      await within(section).findByRole('button', { name: `Rename ${PLAYLIST_NAME}` }),
    )
    fireEvent.change(within(section).getByLabelText('New name'), {
      target: { value: 'A new name' },
    })
    fireEvent.click(within(section).getByRole('button', { name: /^save$/i }))

    expect(await within(section).findByRole('alert')).toHaveTextContent(
      /cannot tell whether the rename went through/,
    )
    expect(within(section).queryByText(/something went wrong/i)).not.toBeInTheDocument()
  })

  // Fails when: the success toast fires on anything but a 200, or is moved to
  // onMutate — the copy would then claim a write Spotify has not confirmed.
  it('only says “renamed” after Spotify has confirmed it', async () => {
    let resolvePatch: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => {
      resolvePatch = resolve
    })

    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = typeof input === 'string' ? input : input.toString()
        const path = new URL(url, 'http://encore.test').pathname
        const method = (init?.method ?? 'GET').toUpperCase()
        const json = (code: number, body: unknown) =>
          new Response(JSON.stringify(body), {
            status: code,
            headers: { 'content-type': 'application/json' },
          })

        if (path === '/api/me') return json(200, ME)
        if (path === '/api/blacklist') return json(200, [])
        if (path === '/api/status') return json(200, status())
        if (path === '/api/playlists' && method === 'GET') return json(200, [playlist()])
        if (path === '/api/playlists/pl-1' && method === 'PATCH') return pending
        return json(404, { error: { code: 'not_found', message: 'No.' } })
      }),
    )

    render(mountSettings())
    const section = await playlistPanel()

    fireEvent.click(
      await within(section).findByRole('button', { name: `Rename ${PLAYLIST_NAME}` }),
    )
    fireEvent.change(within(section).getByLabelText('New name'), {
      target: { value: 'A new name' },
    })
    fireEvent.click(within(section).getByRole('button', { name: /^save$/i }))

    // While Spotify has not answered, the row says it is working and nothing
    // has claimed the rename happened.
    expect(await within(section).findByText('Renaming…')).toBeInTheDocument()
    expect(screen.queryByText(/renamed to/i)).not.toBeInTheDocument()

    resolvePatch(new Response(JSON.stringify(playlist({ name: 'A new name' })), {
      status: 200,
      headers: { 'content-type': 'application/json' },
    }))

    expect(await screen.findByText('Renamed to A new name')).toBeInTheDocument()
  })
})
