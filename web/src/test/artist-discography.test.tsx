/**
 * The artist page's discography panel.
 *
 * Its job is the never-played panel's job one level up, plus one the album page
 * never had: saying what it did *not* count. Coverage counts album_group
 * "album", so "4 of 11" over an artist with 340 singles is an overclaim by
 * omission, and these tests pin the sentence that prevents it as hard as they
 * pin the number.
 *
 * Seven silences to keep apart, not four: Encore has not asked yet, asked and
 * failed, does not ask at all, waited too long, asked and you played everything,
 * asked and there are no albums to count, and its own request still in flight.
 */

import type { ReactElement } from 'react'
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryRouter } from 'react-router-dom'
import { act, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { routes } from '../App'
import { createQueryClient } from '../lib/query'
import type { ArtistDetail, ArtistDiscography, MeResponse } from '../lib/types'
import { DISCOGRAPHY_POLL_START_KEY, discographyPollInterval } from '../pages/ArtistDetail'

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

/**
 * Answers each path with its own body, so one page can be given a whole API.
 * Returns the log of paths asked for, which is how the poll is counted.
 */
function stubRoutes(bodies: Record<string, unknown>): string[] {
  const asked: string[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString()
      const path = new URL(url, 'http://encore.test').pathname
      asked.push(path)
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
  return asked
}

/**
 * Like `stubRoutes`, but one path's response is held open until `resolve` is
 * called. This is what puts the panel's own request in the one state the
 * ordinary stub can never produce — settled nowhere, not even "pending" —
 * which is exactly the frame D4 is about: before the response lands, an
 * instance with fetching turned off and one still asking Spotify look
 * identical, and the panel must not guess which it is.
 */
function stubRoutesWithHeldPath(
  bodies: Record<string, unknown>,
  heldPath: string,
): { asked: string[]; resolveHeld: (body: unknown) => void } {
  const asked: string[] = []
  let release: (body: unknown) => void = () => {}
  const held = new Promise<unknown>((resolve) => {
    release = resolve
  })
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString()
      const path = new URL(url, 'http://encore.test').pathname
      asked.push(path)
      if (path === heldPath) {
        const body = await held
        return new Response(JSON.stringify(body), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        })
      }
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
  return { asked, resolveHeld: release }
}

function mountAt(path: string): ReactElement {
  const router = createMemoryRouter(routes, { initialEntries: [path] })
  return (
    <QueryClientProvider client={createQueryClient()}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}

function artistPayload(): ArtistDetail {
  return {
    artist: {
      id: 'artist-1',
      name: 'A Test Artist',
      imageUrl: '',
      genres: ['post-rock'],
      followers: 1000,
      popularity: 40,
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
    share: 0.1,
    topTracks: [],
    topAlbums: [],
    hourRepartition: [],
    blacklisted: false,
  }
}

function discography(overrides: Partial<ArtistDiscography> = {}): ArtistDiscography {
  return {
    state: 'ready',
    coverage: { covered: 9, total: 11 },
    missing: [
      { id: 'alb-10', name: 'The Tenth', releaseDate: '2022', releasePrecision: 'year' },
      { id: 'alb-11', name: 'The Eleventh', releaseDate: '2024', releasePrecision: 'year' },
    ],
    excluded: { singles: 40, compilations: 3, appearsOn: 7, other: 0 },
    fetchedAt: '2026-07-20T09:00:00Z',
    ...overrides,
  }
}

/** A discography the server will never resolve, which is the state the cap exists for. */
const PENDING: Partial<ArtistDiscography> = {
  state: 'pending',
  coverage: { covered: 0, total: 0 },
  missing: [],
  excluded: { singles: 0, compilations: 0, appearsOn: 0, other: 0 },
  fetchedAt: undefined,
}

async function panel(settled: string | RegExp): Promise<HTMLElement> {
  const heading = await screen.findByRole('heading', { name: 'Albums you have never played' })
  const section = heading.closest('section')
  if (!section) throw new Error('the heading is not inside a panel')
  await within(section).findByText(settled)
  return section
}

function panelNow(): HTMLElement {
  const heading = screen.getByRole('heading', { name: 'Albums you have never played' })
  const section = heading.closest('section')
  if (!section) throw new Error('the heading is not inside a panel')
  return section
}

function discographyCalls(asked: string[]): number {
  return asked.filter((path) => path === '/api/artists/artist-1/discography').length
}

beforeEach(() => {
  vi.unstubAllGlobals()
  window.sessionStorage.clear()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('the discography panel', () => {
  it('names the missing albums and states what it counted', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Tenth')

    expect(within(section).getByText('The Eleventh')).toBeInTheDocument()
    expect(
      within(section).getByText(
        '2 of the 11 albums Spotify lists for this artist have no plays in your history, all time. ' +
          'Singles, compilations and appearances are not counted.',
      ),
    ).toBeInTheDocument()
  })

  // The whole album_group problem in one assertion. Without this line "2 of 11"
  // describes an artist with fifty releases as an artist with eleven.
  it('names what it set aside, with the right plural in every bucket', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Tenth')

    expect(
      within(section).getByText(
        'Spotify also lists 40 singles, 3 compilations and 7 appearances for this artist, ' +
          'which this panel does not count.',
      ),
    ).toBeInTheDocument()
  })

  it('says each excluded bucket in the singular when there is one of it', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        excluded: { singles: 1, compilations: 1, appearsOn: 1, other: 1 },
      }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Tenth')

    expect(
      within(section).getByText(
        'Spotify also lists 1 single, 1 compilation, 1 appearance and 1 other release for this ' +
          'artist, which this panel does not count.',
      ),
    ).toBeInTheDocument()
  })

  it('omits an empty bucket rather than saying "0 compilations"', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        excluded: { singles: 4, compilations: 0, appearsOn: 0, other: 0 },
      }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Tenth')

    expect(
      within(section).getByText(
        'Spotify also lists 4 singles for this artist, which this panel does not count.',
      ),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/0 compilations|0 appearances/)).not.toBeInTheDocument()
  })

  it('says nothing about exclusions when there were none', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        excluded: { singles: 0, compilations: 0, appearsOn: 0, other: 0 },
      }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Tenth')

    expect(within(section).queryByText(/Spotify also lists/)).not.toBeInTheDocument()
    // The description's rule still stands: it is what this panel counts, not a
    // claim that something was excluded.
    expect(
      within(section).getByText(/Singles, compilations and appearances are not counted\./),
    ).toBeInTheDocument()
  })

  // "4 of 11 albums" reads as four albums heard end to end. It is not.
  it('says what counts as having played an album', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Tenth')

    expect(
      within(section).getByText(
        'An album counts as played when you have played any track from it. Albums you played that ' +
          'Spotify does not list under this artist are not counted here.',
      ),
    ).toBeInTheDocument()
  })

  it('agrees with itself when exactly one album is unplayed', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        coverage: { covered: 10, total: 11 },
        missing: [
          { id: 'alb-11', name: 'The Eleventh', releaseDate: '2024', releasePrecision: 'year' },
        ],
      }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Eleventh')

    expect(
      within(section).getByText(
        '1 of the 11 albums Spotify lists for this artist has no plays in your history, all time. ' +
          'Singles, compilations and appearances are not counted.',
      ),
    ).toBeInTheDocument()
    expect(
      within(section).queryByText(/albums Spotify lists for this artist have/),
    ).not.toBeInTheDocument()
  })

  it('does not make a ratio out of a single-album discography', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        coverage: { covered: 0, total: 1 },
        missing: [
          { id: 'alb-1', name: 'The Only One', releaseDate: '2016', releasePrecision: 'year' },
        ],
      }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Only One')

    expect(
      within(section).getByText(
        'The only album Spotify lists for this artist has no plays in your history, all time. ' +
          'Singles, compilations and appearances are not counted.',
      ),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/of the 1 album/)).not.toBeInTheDocument()
  })

  it('says you played something from all of them rather than showing an empty list', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        coverage: { covered: 11, total: 11 },
        missing: [],
      }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('You have played something from every album by this artist')

    expect(within(section).getByText('Spotify lists 11 albums for this artist.')).toBeInTheDocument()
    // Not "you have played every album": coverage counts an album with any play,
    // and the shorter sentence claims eleven records heard end to end.
    expect(within(section).queryByText(/played every album/)).not.toBeInTheDocument()
    // The count line is a double negative when the count is zero, and the body
    // already says the same fact the right way round.
    expect(within(section).queryByText(/\d+ of the \d+ albums/)).not.toBeInTheDocument()
  })

  it('counts a single-album discography correctly when it has been played', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        coverage: { covered: 1, total: 1 },
        missing: [],
      }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('You have played something from every album by this artist')

    expect(within(section).getByText('Spotify lists 1 album for this artist.')).toBeInTheDocument()
    expect(within(section).queryByText(/1 albums/)).not.toBeInTheDocument()
  })

  // The state with no counterpart on the album page. It must not read as a
  // failure and must not read as "you have played everything".
  it('says there are no albums to count, and what there is instead', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        coverage: { covered: 0, total: 0 },
        missing: [],
        excluded: { singles: 12, compilations: 0, appearsOn: 2, other: 0 },
      }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('Spotify lists no albums for this artist')

    expect(
      within(section).getByText(
        'Everything Spotify lists for them is a single, a compilation or an appearance on someone ' +
          "else's record, and this panel counts none of those.",
      ),
    ).toBeInTheDocument()
    // No "also": nothing else was listed for this artist.
    expect(
      within(section).getByText('Spotify lists 12 singles and 2 appearances for this artist.'),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/Spotify also lists/)).not.toBeInTheDocument()
    expect(within(section).queryByText(/played something from every album/)).not.toBeInTheDocument()
    expect(within(section).queryByText(/could not/i)).not.toBeInTheDocument()
    // Nothing was counted, so the sentence about what counting means says nothing.
    expect(within(section).queryByText(/counts as played/)).not.toBeInTheDocument()
  })

  it('says this instance does not fetch discographies, and never blames Spotify', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({ ...PENDING, state: 'disabled' }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('Artist discographies are turned off')

    expect(
      within(section).getByText(
        'This instance does not ask Spotify what an artist has released, so Encore cannot say which ' +
          'of their albums you have never played. Every other figure on this page comes from your ' +
          'own history and is unaffected. An administrator can turn this on with ' +
          'ENCORE_ARTIST_ALBUMS_ENABLED.',
      ),
    ).toBeInTheDocument()
    // An operator's choice is not a Spotify failure, and not a promise to retry.
    expect(within(section).queryByText(/could not/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/tries again/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/failed|error/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/played something from every album/)).not.toBeInTheDocument()
    expect(within(section).queryByText(/Asking Spotify/i)).not.toBeInTheDocument()
    // And it names the right variable. ENCORE_ALBUM_TRACKS_ENABLED is a
    // different switch, and telling an administrator to flip it would leave the
    // panel exactly as it is.
    expect(within(section).queryByText(/ENCORE_ALBUM_TRACKS_ENABLED/)).not.toBeInTheDocument()
  })

  it('says the discography could not be read, and that nothing else is affected', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({ ...PENDING, state: 'unavailable' }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel("This artist's discography could not be read")

    expect(
      within(section).getByText(
        'Encore could not get the list of what this artist has released from Spotify, so it cannot ' +
          'say which of their albums you have never played. Every other figure on this page comes ' +
          'from your own history and is unaffected. Encore tries again later.',
      ),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/played something from every album/)).not.toBeInTheDocument()
    expect(within(section).queryByText(/turned off/i)).not.toBeInTheDocument()
  })

  it('says the discography could not be read when the request itself fails', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
    })
    render(mountAt('/artists/artist-1'))
    await panel("This artist's discography could not be read")
  })

  it('says it is still asking Spotify, and claims nothing about completeness', async () => {
    // On fake timers, because this is the one answer that looks exactly like the
    // panel's own loading frame. Advancing the clock settles the request, so what
    // is asserted is the server having said "pending" and not merely the request
    // being in flight.
    vi.useFakeTimers()
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(PENDING),
    })
    render(mountAt('/artists/artist-1'))
    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    const section = panelNow()

    expect(
      within(section).getByText('Asking Spotify what this artist has released'),
    ).toBeInTheDocument()
    expect(
      within(section).getByText(
        'Encore reads it once and keeps it, so this step is skipped on most visits. The list ' +
          'appears here on its own.',
      ),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/played something from every album/)).not.toBeInTheDocument()
    expect(within(section).queryByText(/\d+ of the \d+ albums/)).not.toBeInTheDocument()
    expect(within(section).queryByText(/could not/i)).not.toBeInTheDocument()
    // Nothing has been counted, so nothing has been excluded either.
    expect(within(section).queryByText(/Spotify also lists/)).not.toBeInTheDocument()
  })

  it('shows a neutral skeleton while its own request is in flight, never "Asking Spotify" on a disabled instance', async () => {
    // Before the response lands, an instance with fetching turned off and one
    // still asking Spotify look identical. Claiming either is a contradiction
    // waiting one round trip to happen.
    const { resolveHeld } = stubRoutesWithHeldPath(
      { '/api/me': ME, '/api/artists/artist-1': artistPayload() },
      '/api/artists/artist-1/discography',
    )
    render(mountAt('/artists/artist-1'))

    const heading = await screen.findByRole('heading', { name: 'Albums you have never played' })
    const section = heading.closest('section')
    if (!section) throw new Error('the heading is not inside a panel')

    expect(within(section).queryByText(/Asking Spotify/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/turned off/i)).not.toBeInTheDocument()
    expect(within(section).getByRole('status')).toBeInTheDocument()

    resolveHeld({ ...discography(PENDING), state: 'disabled' })

    await within(section).findByText('Artist discographies are turned off')
    expect(within(section).queryByText(/Asking Spotify/i)).not.toBeInTheDocument()
  })

  it('serves a discography cached before fetching was turned off, with the date it was read', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({ fetchedAt: '2024-03-12T09:00:00Z' }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Tenth')

    expect(
      within(section).getByText('Discography read from Spotify on 12 Mar 2024.'),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/up to date|current|just now/i)).not.toBeInTheDocument()
  })

  it('carries the read date on the played-everything and no-albums states too', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        coverage: { covered: 11, total: 11 },
        missing: [],
        fetchedAt: '2024-03-12T09:00:00Z',
      }),
    })
    const first = render(mountAt('/artists/artist-1'))
    await panel('Discography read from Spotify on 12 Mar 2024.')
    first.unmount()

    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        coverage: { covered: 0, total: 0 },
        missing: [],
        excluded: { singles: 3, compilations: 0, appearsOn: 0, other: 0 },
        fetchedAt: '2024-03-12T09:00:00Z',
      }),
    })
    render(mountAt('/artists/artist-1'))
    await panel('Discography read from Spotify on 12 Mar 2024.')
  })

  it('shows the release year beside each unplayed album and links to none of them', async () => {
    // Most of these are records nobody has played, so they are not in the
    // catalogue and /albums/{id} would 404 on almost all of them.
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Tenth')

    expect(within(section).getByText('2022')).toBeInTheDocument()
    expect(within(section).getByText('2024')).toBeInTheDocument()
    const row = within(section).getByText('The Tenth').closest('li')
    if (!row) throw new Error('the album name is not in a list row')
    expect(within(row).queryByRole('link')).not.toBeInTheDocument()
  })

  // `missing` has no server-side ceiling: it is bounded only by how many
  // album_group "album" releases Spotify lists, which is hundreds for a
  // prolific artist. The album panel had no equivalent — a record's track count
  // capped it naturally — so the length is handled here rather than inherited.
  it('keeps the disclosures reachable when the list runs to hundreds of albums', async () => {
    const many = Array.from({ length: 240 }, (_, i) => ({
      id: `alb-${i}`,
      name: `Record ${i}`,
      releaseDate: '2020',
      releasePrecision: 'year',
    }))
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        coverage: { covered: 0, total: 240 },
        missing: many,
      }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('Record 0')

    expect(within(section).getByText('Record 239')).toBeInTheDocument()
    // The rows scroll inside their own box; the sentences that qualify the
    // number do not, or 240 rows would push every one of them out of sight and
    // the exclusion would be present in the DOM and absent from the screen.
    const list = within(section).getByRole('list')
    expect(list.className).toContain('overflow-y-auto')
    const disclosures = [
      within(section).getByText(
        'Spotify also lists 40 singles, 3 compilations and 7 appearances for this artist, ' +
          'which this panel does not count.',
      ),
      within(section).getByText(
        'An album counts as played when you have played any track from it. Albums you played that ' +
          'Spotify does not list under this artist are not counted here.',
      ),
      within(section).getByText('Discography read from Spotify on 20 Jul 2026.'),
    ]
    for (const line of disclosures) expect(list.contains(line)).toBe(false)
  })
})

/**
 * The panel's description sits above seven different bodies, so anything it
 * asserts has to be true of all seven. This is where 2e-i lost a review round:
 * a description that quietly reinstated a Spotify provenance directly above the
 * one body that had just been careful not to blame Spotify. No negative
 * assertion inside an individual body's test can catch that, because it is a
 * positive false claim rather than a failure phrasing.
 */
describe('the panel description, read together with the body under it', () => {
  const BODIES: [string, Partial<ArtistDiscography>, string][] = [
    ['nothing asked yet', PENDING, 'Asking Spotify what this artist has released'],
    [
      'a recorded failure',
      { ...PENDING, state: 'unavailable' },
      "This artist's discography could not be read",
    ],
    ['fetching turned off', { ...PENDING, state: 'disabled' }, 'Artist discographies are turned off'],
    [
      'everything played',
      { coverage: { covered: 11, total: 11 }, missing: [] },
      'You have played something from every album by this artist',
    ],
    [
      'no albums to count',
      {
        coverage: { covered: 0, total: 0 },
        missing: [],
        excluded: { singles: 3, compilations: 0, appearsOn: 0, other: 0 },
      },
      'Spotify lists no albums for this artist',
    ],
  ]

  it.each(BODIES)('claims nothing untrue above %s', async (_label, overrides, body) => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(overrides),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel(body)

    expect(
      within(section).getByText(
        "Which of this artist's albums have no plays in your history, all time. Singles, " +
          'compilations and appearances are not counted.',
      ),
    ).toBeInTheDocument()
    // Nothing has been read above four of these five, so nothing may say one was.
    expect(within(section).queryByText(/read once and kept/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/From Spotify's own list/i)).not.toBeInTheDocument()
  })
})

describe('the discography poll', () => {
  it('polls only while a walk is running, and not once it has given up', () => {
    expect(discographyPollInterval('pending')).toBe(3000)
    expect(discographyPollInterval('pending', false)).toBe(3000)
    expect(discographyPollInterval('pending', true)).toBe(false)
    expect(discographyPollInterval('ready')).toBe(false)
    expect(discographyPollInterval('unavailable')).toBe(false)
    expect(discographyPollInterval('disabled')).toBe(false)
    expect(discographyPollInterval(undefined)).toBe(false)
  })

  it('asks again every three seconds while the answer is pending', async () => {
    vi.useFakeTimers()
    const asked = stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(PENDING),
    })
    render(mountAt('/artists/artist-1'))

    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    expect(discographyCalls(asked)).toBe(1)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(3_100)
    })
    expect(discographyCalls(asked)).toBe(2)
  })

  it('stops asking at the cap, and says so without blaming Spotify', async () => {
    vi.useFakeTimers()
    const asked = stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(PENDING),
    })
    render(mountAt('/artists/artist-1'))

    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    expect(within(panelNow()).getByText('Asking Spotify what this artist has released')).toBeVisible()

    // Just short of three minutes. Advanced in stages rather than one jump
    // because `act` holds React's updates until its callback returns, so a single
    // leap past the cap would run every interval tick before the component heard
    // that the cap passed — an artefact of the harness, not of the page.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(178_000)
    })
    const beforeCap = discographyCalls(asked)
    expect(beforeCap).toBeGreaterThan(50)
    expect(
      within(panelNow()).getByText('Asking Spotify what this artist has released'),
    ).toBeInTheDocument()

    await act(async () => {
      await vi.advanceTimersByTimeAsync(3_500)
    })
    const settled = discographyCalls(asked)
    expect(settled).toBeLessThanOrEqual(beforeCap + 2)
    const capped = panelNow()
    expect(within(capped).getByText('No discography for this artist yet')).toBeInTheDocument()
    expect(
      within(capped).getByText(
        "Encore waited three minutes for this artist's discography and has stopped for now; it may " +
          'still arrive — reopen this page to check. Every other figure on this page comes from ' +
          'your own history and is unaffected.',
      ),
    ).toBeInTheDocument()
    expect(
      within(capped).queryByText('Asking Spotify what this artist has released'),
    ).not.toBeInTheDocument()
    // Running out of patience is not a refusal, and what causes it is very likely
    // local — a claim that errors persists nothing and re-enters "pending" for
    // ever — so Spotify is not named as the party that would not answer.
    expect(within(capped).queryByText(/could not/i)).not.toBeInTheDocument()
    expect(within(capped).queryByText(/tries again/i)).not.toBeInTheDocument()
    expect(within(capped).queryByText(/Spotify/)).not.toBeInTheDocument()

    // And having given up, it stays given up.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(180_000)
    })
    expect(discographyCalls(asked)).toBe(settled)
  })

  it('does not restart the cap when the tab is reloaded near it', async () => {
    window.sessionStorage.setItem(
      `${DISCOGRAPHY_POLL_START_KEY}artist-1`,
      String(Date.now() - 300_000),
    )
    const asked = stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(PENDING),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('No discography for this artist yet')

    expect(
      within(section).queryByText('Asking Spotify what this artist has released'),
    ).not.toBeInTheDocument()
    expect(discographyCalls(asked)).toBe(1)
  })

  it('forgets the cap once the artist resolves, so a later pending artist starts fresh', async () => {
    window.sessionStorage.setItem(`${DISCOGRAPHY_POLL_START_KEY}artist-1`, String(Date.now() - 1_000))
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(),
    })
    render(mountAt('/artists/artist-1'))
    await panel('The Tenth')

    expect(window.sessionStorage.getItem(`${DISCOGRAPHY_POLL_START_KEY}artist-1`)).toBeNull()
  })

  it('does not share a cap with the album page', async () => {
    // The two panels key their windows by different prefixes, so one artist's
    // stuck walk cannot make an album with the same id report having given up.
    window.sessionStorage.setItem(
      'encore.tracklist-poll-start.artist-1',
      String(Date.now() - 300_000),
    )
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(PENDING),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('Asking Spotify what this artist has released')

    // Asserted on the recorded window rather than only on the words, because
    // "still asking" is the frame this panel opens on: a shared prefix would
    // give up one tick later, which a findByText that resolves on the first
    // matching render would not notice. The window this panel opened is its
    // own, and it opened it now.
    const own = Number(window.sessionStorage.getItem(`${DISCOGRAPHY_POLL_START_KEY}artist-1`))
    expect(Date.now() - own).toBeLessThan(60_000)
    expect(
      within(section).queryByText('No discography for this artist yet'),
    ).not.toBeInTheDocument()
  })
})
