/**
 * The album page's "never played" panel.
 *
 * Its whole job is to keep four different silences apart: Encore has not asked
 * Spotify yet, Encore asked and failed, this instance does not ask at all, and
 * Encore asked and you have played everything. An empty list means all four, so
 * these tests pin the copy rather than the emptiness.
 *
 * The poll is pinned here too, and for a reason that is not obvious from the
 * client: "pending" is unbounded on the server. Nothing in the payload says how
 * long it has held, and a claim against album_track_fetches that errors leaves
 * no record, so the very next request lands back in "pending" for ever. If the
 * cap below stops working, a tab left open asks the API every two seconds until
 * somebody closes it.
 */

import type { ReactElement } from 'react'
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryRouter } from 'react-router-dom'
import { act, render, screen, within } from '@testing-library/react'
import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import { routes } from '../App'
import { createQueryClient } from '../lib/query'
import type { AlbumDetail, AlbumTrackList, MeResponse } from '../lib/types'
import { TRACKLIST_POLL_START_KEY, tracklistPollInterval } from '../pages/AlbumDetail'

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
 * The album record. `totalTracks` is a separate parameter because it is a
 * separate fact from `completion`: it comes from enrichment on this endpoint,
 * while the panel's own denominator comes from /tracklist, and the whole point
 * of the reconciliation line is that the two are allowed to differ.
 */
function albumPayload(completion: AlbumDetail['completion'], totalTracks = 12): AlbumDetail {
  return {
    album: {
      id: 'album-1',
      name: 'A Test Record',
      imageUrl: '',
      releaseDate: '2016-05-20',
      releasePrecision: 'day',
      albumType: 'album',
      totalTracks,
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

function mountAt(path: string): ReactElement {
  const router = createMemoryRouter(routes, { initialEntries: [path] })
  return (
    <QueryClientProvider client={createQueryClient()}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}

function tracklist(overrides: Partial<AlbumTrackList> = {}): AlbumTrackList {
  return {
    state: 'ready',
    coverage: { covered: 10, total: 12 },
    missing: [
      { id: 'track-11', name: 'The Eleventh', discNumber: 1, trackNumber: 11 },
      { id: 'track-12', name: 'The Twelfth', discNumber: 1, trackNumber: 12 },
    ],
    fetchedAt: '2026-07-20T09:00:00Z',
    ...overrides,
  }
}

/** A listing the server will never resolve, which is the state the cap exists for. */
const PENDING: Partial<AlbumTrackList> = {
  state: 'pending',
  coverage: { covered: 0, total: 0 },
  missing: [],
  fetchedAt: undefined,
}

/**
 * The panel once it is showing `settled`, found the way a person finds it: by
 * its heading.
 *
 * The wait is on a line of the answer rather than on the heading, because the
 * heading is there from the first frame — the panel renders "asking Spotify"
 * while its own request is still in flight, deliberately, since a listing being
 * fetched and a listing not yet asked for are the same thing to a reader.
 */
async function panel(settled: string | RegExp): Promise<HTMLElement> {
  const heading = await screen.findByRole('heading', { name: 'Tracks you have never played' })
  const section = heading.closest('section')
  if (!section) throw new Error('the heading is not inside a panel')
  await within(section).findByText(settled)
  return section
}

/** The same panel, without waiting — for the fake-timer tests, which drive their own clock. */
function panelNow(): HTMLElement {
  const heading = screen.getByRole('heading', { name: 'Tracks you have never played' })
  const section = heading.closest('section')
  if (!section) throw new Error('the heading is not inside a panel')
  return section
}

function tracklistCalls(asked: string[]): number {
  return asked.filter((path) => path === '/api/albums/album-1/tracklist').length
}

beforeEach(() => {
  vi.unstubAllGlobals()
  window.sessionStorage.clear()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('the never-played panel', () => {
  it('names the missing tracks and states the listing as its denominator', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist(),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel('The Eleventh')

    expect(within(section).getByText('The Twelfth')).toBeInTheDocument()
    expect(
      within(section).getByText(
        '2 of the 12 tracks Spotify lists for this album have no plays in your history, all time.',
      ),
    ).toBeInTheDocument()
  })

  it('says you played everything rather than showing an empty list', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 12, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist({
        coverage: { covered: 12, total: 12 },
        missing: [],
      }),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel('You have played every track on this album')

    expect(
      within(section).getByText('Spotify lists 12 tracks for this album.'),
    ).toBeInTheDocument()
    // The count line is a double negative when the count is zero, and the body
    // already says the same fact the right way round.
    expect(within(section).queryByText(/\d+ of the \d+ tracks/)).not.toBeInTheDocument()
  })

  it('says it is still asking Spotify, and claims nothing about completeness', async () => {
    // On fake timers, because this is the one answer that looks exactly like
    // the panel's own loading frame. Advancing the clock settles the request,
    // so what is asserted below is the server having said "pending" and not
    // merely the request being in flight.
    vi.useFakeTimers()
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist(PENDING),
    })
    render(mountAt('/albums/album-1'))
    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    const section = panelNow()

    expect(
      within(section).getByText("Asking Spotify for this album's track list"),
    ).toBeInTheDocument()
    expect(
      within(section).getByText(
        'Encore reads it once and keeps it, so this happens only the first time somebody opens this album. The list appears here on its own.',
      ),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/played every track/i)).not.toBeInTheDocument()
    // Nothing counted, nothing claimed: no listing has arrived to count.
    expect(within(section).queryByText(/\d+ of the \d+ tracks/)).not.toBeInTheDocument()
    expect(within(section).queryByText(/could not/i)).not.toBeInTheDocument()
  })

  it('says this instance does not fetch track lists, and never blames Spotify', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist({ ...PENDING, state: 'disabled' }),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel('Album track lists are turned off')

    expect(
      within(section).getByText(
        'This instance does not ask Spotify what is on an album, so Encore cannot say which tracks you have never played. Everything else on this page comes from your own listening and is unaffected. An administrator can turn this on with ENCORE_ALBUM_TRACKS_ENABLED.',
      ),
    ).toBeInTheDocument()
    // An operator's choice is not a Spotify failure, and not a promise to retry.
    expect(within(section).queryByText(/could not/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/tries again/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/failed|error/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/played every track/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/Asking Spotify/i)).not.toBeInTheDocument()
  })

  it('serves a listing cached before fetching was turned off, with the date it was read', async () => {
    // The server reports "ready" whenever a listing exists, switch or no switch;
    // the date is what stops an unrefreshable list from reading as current.
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist({ fetchedAt: '2024-03-12T09:00:00Z' }),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel('The Eleventh')

    expect(
      within(section).getByText('Track list read from Spotify on 12 Mar 2024.'),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/up to date|current|just now/i)).not.toBeInTheDocument()
  })

  it('carries the read date on the played-everything state too', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 12, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist({
        coverage: { covered: 12, total: 12 },
        missing: [],
        fetchedAt: '2024-03-12T09:00:00Z',
      }),
    })
    render(mountAt('/albums/album-1'))
    await panel('Track list read from Spotify on 12 Mar 2024.')
  })

  it('says the list could not be read, and that nothing else on the page is affected', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist({ ...PENDING, state: 'unavailable' }),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel("This album's track list could not be read")

    expect(
      within(section).getByText(
        'Encore could not get the list of what is on this album from Spotify, so it cannot say which tracks you have never played. Every other figure on this page comes from your own history and is unaffected. Encore tries again later.',
      ),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/played every track/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/turned off/i)).not.toBeInTheDocument()
  })

  it('says the list could not be read when the request itself fails', async () => {
    // album-completion.test.tsx exercises this by omission: it never stubs the
    // tracklist route at all, so the panel must degrade rather than throw.
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
    })
    render(mountAt('/albums/album-1'))
    await panel("This album's track list could not be read")
  })

  it('prints both numbers when the listing and the album record disagree', async () => {
    stubRoutes({
      '/api/me': ME,
      // album.totalTracks is 12 in the shared fixture.
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist({
        coverage: { covered: 10, total: 14 },
        missing: [
          { id: 'track-11', name: 'The Eleventh', discNumber: 1, trackNumber: 11 },
          { id: 'track-12', name: 'The Twelfth', discNumber: 1, trackNumber: 12 },
          { id: 'track-13', name: 'A Bonus', discNumber: 1, trackNumber: 13 },
          { id: 'track-14', name: 'Another Bonus', discNumber: 1, trackNumber: 14 },
        ],
      }),
    })
    render(mountAt('/albums/album-1'))
    await panel("The album record says 12. This panel follows Spotify's list.")
  })

  it('reconciles the two numbers on the played-everything state, and says each once', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 12, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist({
        coverage: { covered: 14, total: 14 },
        missing: [],
      }),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel('You have played every track on this album')

    expect(
      within(section).getByText(
        'Spotify lists 14 tracks for this album; the album record says 12. This panel follows the list.',
      ),
    ).toBeInTheDocument()
    // The reconciliation already names the listing's total, so the count line
    // is not printed beside it saying the same number over again.
    expect(
      within(section).queryByText('Spotify lists 14 tracks for this album.'),
    ).not.toBeInTheDocument()
    expect(
      within(section).getByText('Track list read from Spotify on 20 Jul 2026.'),
    ).toBeInTheDocument()
  })

  it('says nothing about the album record when the two numbers agree', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist(),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel('The Eleventh')

    expect(within(section).queryByText(/the album record/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/follows the list/i)).not.toBeInTheDocument()
  })

  it('names the album record as the thing that is missing, not the listing', async () => {
    // The identity panel above says "Tracks — Not known" on the same screen. A
    // confident "12 tracks" under it with no explanation is the same defect as
    // rendering "Tracks: 0" over "Track count not known yet".
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 0, total: 0, known: false }, 0),
      '/api/albums/album-1/tracklist': tracklist(),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel('The album record has no track count yet.')

    // There is no rival number to follow over, so it does not offer to.
    expect(within(section).queryByText(/follows/i)).not.toBeInTheDocument()
  })

  it('agrees with itself when exactly one track is unplayed', async () => {
    // One missing of twelve is among the commonest things this panel reports,
    // and the plural verb reads as a mistake the moment the count is one.
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 11, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist({
        coverage: { covered: 11, total: 12 },
        missing: [{ id: 'track-12', name: 'The Twelfth', discNumber: 1, trackNumber: 12 }],
      }),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel('The Twelfth')

    expect(
      within(section).getByText(
        '1 of the 12 tracks Spotify lists for this album has no plays in your history, all time.',
      ),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/tracks Spotify lists for this album have/)).not.toBeInTheDocument()
  })

  it('does not make a ratio out of a single-track listing', async () => {
    // "1 of the 1 track" is not a sentence, and a single is a whole release.
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 0, total: 1, known: true }, 1),
      '/api/albums/album-1/tracklist': tracklist({
        coverage: { covered: 0, total: 1 },
        missing: [{ id: 'track-1', name: 'The Only One', discNumber: 1, trackNumber: 1 }],
      }),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel('The Only One')

    expect(
      within(section).getByText(
        'The only track Spotify lists for this album has no plays in your history, all time.',
      ),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/of the 1 track/)).not.toBeInTheDocument()
  })

  it('counts a single-track listing correctly when it has been played', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 1, total: 1, known: true }, 1),
      '/api/albums/album-1/tracklist': tracklist({
        coverage: { covered: 1, total: 1 },
        missing: [],
      }),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel('You have played every track on this album')

    expect(
      within(section).getByText('Spotify lists 1 track for this album.'),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/1 tracks/)).not.toBeInTheDocument()
  })

  it('numbers every row by disc, or none of them', async () => {
    // Showing the disc only from the second one gives a column reading 11, 12,
    // 2.3 \u2014 which looks like a decimal and sorts wrong to the eye.
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 20, known: true }, 20),
      '/api/albums/album-1/tracklist': tracklist({
        coverage: { covered: 18, total: 20 },
        missing: [
          { id: 'track-a', name: 'On Disc One', discNumber: 1, trackNumber: 11 },
          { id: 'track-b', name: 'On Disc Two', discNumber: 2, trackNumber: 3 },
        ],
      }),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel('On Disc One')

    expect(within(section).getByText('1-11')).toBeInTheDocument()
    expect(within(section).getByText('2-3')).toBeInTheDocument()
    expect(within(section).queryByText('2.3')).not.toBeInTheDocument()
    expect(within(section).queryByText('11')).not.toBeInTheDocument()
  })

  it('leaves the completion figure alone when the track count is unknown', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 0, total: 0, known: false }),
      '/api/albums/album-1/tracklist': tracklist({
        coverage: { covered: 10, total: 12 },
      }),
    })
    render(mountAt('/albums/album-1'))
    // Wait for the listing to have landed: back-filling the figure from it is
    // only possible once it is there, and this test exists to catch that.
    await panel('The Eleventh')

    const heard = screen.getByRole('heading', { name: 'Heard' }).closest('section')
    if (!heard) throw new Error('no Heard panel')
    // An unresolved track count is still unresolved. A cached listing is not a
    // licence to compute a percentage the enrichment has not earned.
    expect(within(heard).getByText(/track count not known yet/i)).toBeInTheDocument()
    expect(within(heard).queryByText(/%/)).not.toBeInTheDocument()
    expect(within(heard).queryByText(/\d+ of \d+/)).not.toBeInTheDocument()
  })
})

/**
 * The panel's description sits above four different bodies, so anything it
 * asserts has to be true of all four. An earlier draft said the listing had
 * been "read once and kept", which is false on `disabled` — nothing has ever
 * been read and nothing ever will be — and reinstated a Spotify provenance
 * directly above the one body that had just been careful not to blame Spotify.
 * No negative assertion in the `disabled` test could catch that, because it is
 * a positive false claim rather than a failure phrasing, so it is pinned here
 * against every body it can appear above.
 */
describe('the panel description, read together with the body under it', () => {
  const BODIES: [string, Partial<AlbumTrackList>, string][] = [
    ['nothing asked yet', PENDING, "Asking Spotify for this album's track list"],
    [
      'a recorded failure',
      { ...PENDING, state: 'unavailable' },
      "This album's track list could not be read",
    ],
    ['fetching turned off', { ...PENDING, state: 'disabled' }, 'Album track lists are turned off'],
    [
      'everything played',
      { coverage: { covered: 12, total: 12 }, missing: [] },
      'You have played every track on this album',
    ],
  ]

  it.each(BODIES)('claims nothing about a read above %s', async (_label, overrides, body) => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist(overrides),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel(body)

    expect(
      within(section).getByText(
        'Which tracks on this record have no plays in your history, all time.',
      ),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/read once and kept/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/From Spotify's own list/i)).not.toBeInTheDocument()
  })
})

describe('the tracklist poll', () => {
  it('polls only while a fetch is running, and not once it has given up', () => {
    expect(tracklistPollInterval('pending')).toBe(2000)
    expect(tracklistPollInterval('pending', false)).toBe(2000)
    // The cap. Nothing on the server ends "pending", so this has to.
    expect(tracklistPollInterval('pending', true)).toBe(false)
    expect(tracklistPollInterval('ready')).toBe(false)
    expect(tracklistPollInterval('unavailable')).toBe(false)
    expect(tracklistPollInterval('disabled')).toBe(false)
    expect(tracklistPollInterval(undefined)).toBe(false)
  })

  it('asks again every two seconds while the answer is pending', async () => {
    vi.useFakeTimers()
    const asked = stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist(PENDING),
    })
    render(mountAt('/albums/album-1'))

    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    expect(tracklistCalls(asked)).toBe(1)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_100)
    })
    expect(tracklistCalls(asked)).toBe(2)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_100)
    })
    expect(tracklistCalls(asked)).toBe(3)
  })

  it('stops asking at the cap, and falls back to the copy for a list it does not have', async () => {
    vi.useFakeTimers()
    const asked = stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist(PENDING),
    })
    render(mountAt('/albums/album-1'))

    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    expect(within(panelNow()).getByText("Asking Spotify for this album's track list")).toBeVisible()

    // Just short of two minutes: still asking, still saying so. The clock is
    // advanced in stages rather than one jump because `act` holds React's
    // updates until its callback returns, so a single 125-second leap would run
    // every interval tick before the component ever heard that the cap passed —
    // an artefact of the harness, not of the page.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(119_000)
    })
    const beforeCap = tracklistCalls(asked)
    expect(beforeCap).toBeGreaterThan(50)
    expect(
      within(panelNow()).getByText("Asking Spotify for this album's track list"),
    ).toBeInTheDocument()

    // Past it. The server would answer "pending" for ever; this is what stops.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_500)
    })
    const settled = tracklistCalls(asked)
    expect(settled).toBeLessThanOrEqual(beforeCap + 2)
    const capped = panelNow()
    expect(within(capped).getByText('No track list for this album yet')).toBeInTheDocument()
    expect(
      within(capped).getByText(
        "Encore has been waiting two minutes for this album's track list and has stopped waiting for now \u2014 it may still arrive. Every other figure on this page comes from your own history and is unaffected.",
      ),
    ).toBeInTheDocument()
    expect(
      within(capped).queryByText("Asking Spotify for this album's track list"),
    ).not.toBeInTheDocument()
    // Running out of patience is not a refusal. A claim that errors server-side
    // records nothing and re-enters "pending" for ever, so two minutes of it is
    // better evidence of a local fault than of Spotify declining to answer.
    expect(within(capped).queryByText(/could not/i)).not.toBeInTheDocument()
    expect(within(capped).queryByText(/tries again/i)).not.toBeInTheDocument()
    expect(within(capped).queryByText(/Spotify/)).not.toBeInTheDocument()

    // And having given up, it stays given up: another two minutes of a tab left
    // open costs the API nothing.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(120_000)
    })
    expect(tracklistCalls(asked)).toBe(settled)
  })

  it('does not restart the cap when the tab is reloaded near it', async () => {
    // A reload rebuilds every component and every query, so an in-memory clock
    // would begin its two minutes again and a page left open through a few
    // reloads would never stop asking. The start is in sessionStorage for this.
    window.sessionStorage.setItem(
      `${TRACKLIST_POLL_START_KEY}album-1`,
      String(Date.now() - 200_000),
    )
    const asked = stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist(PENDING),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel('No track list for this album yet')

    expect(
      within(section).queryByText("Asking Spotify for this album's track list"),
    ).not.toBeInTheDocument()
    expect(tracklistCalls(asked)).toBe(1)
  })

  it('opens a fresh window when the recorded one belongs to an earlier visit', async () => {
    // Persisting the start stops a reload restarting the cap, but only for as
    // long as it is plausibly the same sitting. Twenty minutes later the server
    // may well have started a healthy fetch, and refusing to ask on the first
    // frame would hide a listing that is already there.
    window.sessionStorage.setItem(
      `${TRACKLIST_POLL_START_KEY}album-1`,
      String(Date.now() - 20 * 60_000),
    )
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist(PENDING),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel("Asking Spotify for this album's track list")

    expect(within(section).queryByText('No track list for this album yet')).not.toBeInTheDocument()
    const restarted = Number(window.sessionStorage.getItem(`${TRACKLIST_POLL_START_KEY}album-1`))
    expect(Date.now() - restarted).toBeLessThan(60_000)
  })

  it('forgets the cap once the album resolves, so a later pending album starts fresh', async () => {
    window.sessionStorage.setItem(`${TRACKLIST_POLL_START_KEY}album-1`, String(Date.now() - 1_000))
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist(),
    })
    render(mountAt('/albums/album-1'))
    await panel('The Eleventh')

    expect(window.sessionStorage.getItem(`${TRACKLIST_POLL_START_KEY}album-1`)).toBeNull()
  })
})
