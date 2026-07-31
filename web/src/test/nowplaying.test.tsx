/**
 * The now-playing card, one sentence at a time.
 *
 * Almost every assertion here is a copy assertion, because almost every way this
 * card can be wrong is a sentence rather than a crash. The two it exists to keep
 * apart are worth naming: a null observation means *Encore has not managed to
 * look*, and an observation whose state is `idle` means *nothing is playing*.
 * Telling somebody their player is silent when nobody has looked is a claim
 * about their evening that nobody checked, and no type in the payload can stop
 * it — only these tests can.
 *
 * Ages are built relative to `Date.now()` rather than pinned to a fixed instant,
 * so `formatRelative` produces exactly one phrase and the whole sentence can be
 * asserted. "The last check failed 3 minutes ago." is the assertion; a regex on
 * its prefix would pass for a card that said nothing about how stale it is.
 */

import type { ReactElement } from 'react'
import type { QueryClient } from '@tanstack/react-query'
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryRouter } from 'react-router-dom'
import { act, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { routes } from '../App'
import { createQueryClient, qk } from '../lib/query'
import type { MeResponse, NowPlaying, NowPlayingObservation, Summary } from '../lib/types'
import { nowPlayingPollInterval } from '../pages/Dashboard'

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
    scopes: ['user-read-recently-played', 'user-read-playback-state'],
    missingScopes: [],
  },
  csrfToken: 'not-a-real-token',
  listening: {
    firstListenAt: '2019-03-04T12:00:00.000Z',
    lastListenAt: '2026-07-26T09:00:00.000Z',
  },
  instance: { registrationsEnabled: false, version: '1.0.0' },
}

const SUMMARY: Summary = {
  listens: 412,
  distinctTracks: 240,
  distinctArtists: 88,
  distinctAlbums: 96,
  msPlayed: 90_000_000,
  activeDays: 21,
  firstListenAt: '2019-03-04T12:00:00.000Z',
  lastListenAt: '2026-07-26T09:00:00.000Z',
}

/**
 * The least that makes the dashboard render its populated body, which is the
 * only body the card appears in.
 *
 * Deliberately nothing else: the other panels answer 404 and show their own
 * error states, which is noise the card is scoped away from by `within` and
 * which keeps this suite from re-fixturing eleven endpoints it never asserts on.
 */
const DASHBOARD_BODIES: Record<string, unknown> = {
  '/api/me': ME,
  '/api/stats/summary': SUMMARY,
}

/** Answers each path with its own body, and returns the log of paths asked for. */
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
 * Like `stubRoutes`, but one path's response is held open until it is released.
 *
 * This is the only way to observe the card's loading frame: the ordinary stub
 * settles in the same tick the dashboard body renders in, so the skeleton is
 * never on screen long enough for an assertion to see it.
 */
function stubRoutesWithHeldPath(
  bodies: Record<string, unknown>,
  heldPath: string,
): (body: unknown) => void {
  let release: (body: unknown) => void = () => {}
  const held = new Promise<unknown>((resolve) => {
    release = resolve
  })
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString()
      const path = new URL(url, 'http://encore.test').pathname
      const body = path === heldPath ? await held : bodies[path]
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
  return release
}

function mountAt(path: string, client: QueryClient = createQueryClient()): ReactElement {
  const router = createMemoryRouter(routes, { initialEntries: [path] })
  return (
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}

/** An instant a given distance in the past, so `formatRelative` has one answer. */
function ago(ms: number): string {
  return new Date(Date.now() - ms).toISOString()
}

function payload(overrides: Partial<NowPlaying> = {}): NowPlaying {
  return {
    enabled: true,
    intervalSeconds: 30,
    scopeGranted: true,
    checkedAt: ago(5_000),
    failed: false,
    observation: null,
    ...overrides,
  }
}

function observation(overrides: Partial<NowPlayingObservation> = {}): NowPlayingObservation {
  return {
    observedAt: ago(5_000),
    state: 'playing',
    kind: 'track',
    title: 'The Wheel',
    artist: 'SOHN',
    trackId: 'spotifytrack00000001',
    progressMs: 161_000,
    durationMs: 255_000,
    deviceName: 'Kitchen speaker',
    ...overrides,
  }
}

/** A player holding nothing at all, which is not the same as never having looked. */
function silence(overrides: Partial<NowPlayingObservation> = {}): NowPlayingObservation {
  return observation({
    state: 'idle',
    kind: 'none',
    title: '',
    artist: '',
    trackId: '',
    progressMs: null,
    durationMs: null,
    deviceName: '',
    ...overrides,
  })
}

/** An advert, or anything else Spotify will not describe. It carries no name. */
function unidentifiable(overrides: Partial<NowPlayingObservation> = {}): NowPlayingObservation {
  return observation({
    kind: 'unknown',
    title: '',
    artist: '',
    trackId: '',
    progressMs: null,
    durationMs: null,
    ...overrides,
  })
}

/**
 * Renders the dashboard with a now-playing payload and returns the settled card.
 *
 * The heading is on screen from the first frame, before the card's own request
 * has resolved, so waiting only for it would let every assertion below run
 * against the skeleton. Waiting for the skeleton to go is what makes them
 * assertions about an answer rather than about the wait for one.
 */
async function card(np: NowPlaying): Promise<HTMLElement> {
  stubRoutes({ ...DASHBOARD_BODIES, '/api/nowplaying': np })
  render(mountAt('/'))
  return await settledCard()
}

async function settledCard(): Promise<HTMLElement> {
  const heading = await screen.findByRole('heading', { name: 'Now playing' })
  const section = heading.closest('section')
  if (!section) throw new Error('the heading is not inside a panel')
  await waitFor(() => {
    expect(within(section).queryByText('Loading what you are playing')).not.toBeInTheDocument()
  })
  return section
}

/**
 * The same, on fake timers, which cannot use `findBy` at all.
 *
 * A fixed `advanceTimersByTimeAsync(100)` is not enough, and the way it is not
 * enough is invisible in a full-file run: the dashboard is a lazily imported
 * chunk and its body renders only once the summary query lands, neither of which
 * is a timer. After forty other tests in this file the module is warm and 100ms
 * is plenty; run alone, it is not, and the card is simply not on screen yet. The
 * first draft of this suite's failed-poll test asserted against that and failed
 * on its own while passing in the file — an order-dependent test, which can
 * verify nothing, and which faked two mutation kills before it was caught.
 */
async function settledCardWhileTicking(): Promise<HTMLElement> {
  for (let tick = 0; tick < 100; tick += 1) {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10)
    })
    const section = screen.queryByRole('heading', { name: 'Now playing' })?.closest('section')
    if (section && !within(section).queryByText('Loading what you are playing')) return section
  }
  // A whole second of fake time, still short of the shortest poll interval, so
  // reaching here is a card that never settled rather than a clock that ran on.
  throw new Error('the now-playing card never settled')
}

beforeEach(() => {
  vi.unstubAllGlobals()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('the now-playing card', () => {
  // The panel's description is constant across every body below, so it can
  // never contradict one. It also states the read-only promise where somebody
  // reading the card will see it.
  //
  // Fails when: the description is made conditional, or the promise is dropped.
  it('states what it is, and that nothing here becomes history', async () => {
    const section = await card(payload({ observation: observation() }))
    expect(
      within(section).getByText(
        'What Spotify says you are playing. Nothing here is added to your listening history.',
      ),
    ).toBeInTheDocument()
  })

  // The dashboard is the home screen; a panel saying "turned off" on every load
  // for ever is a nag about a decision the listener cannot change. Settings says
  // it instead.
  //
  // The wait is on the populated body *and* on the answer reaching the cache.
  // Waiting only for the h1 would assert against the whole-page loading frame,
  // which renders no body at all — and would pass with the guard deleted.
  //
  // Fails when: the `enabled === false` guard around the card is removed.
  it('is not rendered at all when the instance does not poll', async () => {
    const client = createQueryClient()
    stubRoutes({
      ...DASHBOARD_BODIES,
      '/api/nowplaying': payload({ enabled: false, intervalSeconds: 0 }),
    })
    render(mountAt('/', client))

    await screen.findByRole('heading', { name: 'Recently played' })
    await waitFor(() => {
      expect(client.getQueryData(qk.nowPlaying())).toBeDefined()
    })

    expect(screen.queryByRole('heading', { name: 'Now playing' })).not.toBeInTheDocument()
  })

  // The frame before the answer arrives. It has to claim nothing at all: an
  // instance that does not poll and one that is simply slow look identical from
  // here, and guessing between them puts a false sentence on screen and then
  // contradicts it.
  //
  // Fails when: the card is gated on `enabled === true`, which makes this frame
  // unrenderable; or the skeleton branch is replaced with any of the answers.
  it('claims nothing while it is still asking', async () => {
    const release = stubRoutesWithHeldPath(DASHBOARD_BODIES, '/api/nowplaying')
    render(mountAt('/'))

    const heading = await screen.findByRole('heading', { name: 'Now playing' })
    const section = heading.closest('section')
    if (!section) throw new Error('the heading is not inside a panel')

    expect(within(section).getByText('Loading what you are playing')).toBeInTheDocument()
    expect(within(section).getByRole('status')).toHaveAttribute('aria-busy', 'true')
    expect(within(section).queryByText(/turned off/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/nothing is playing/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/has not checked/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/could not be loaded/i)).not.toBeInTheDocument()

    release(payload({ observation: observation() }))
    expect(await within(section).findByText('SOHN')).toBeInTheDocument()
  })

  // A request that fails is a fourth thing again: not off, not idle, not
  // unlooked-at. It is the one state on this card where trying again can help,
  // so it is the one state that offers it.
  //
  // Fails when: the error branch is dropped — the card then renders the null
  // body and says nothing at all where a failure happened.
  it('says the card itself could not be loaded, and offers a retry', async () => {
    // No `/api/nowplaying` body at all: the stub answers 404, which is also what
    // an older server without the endpoint would do.
    stubRoutes(DASHBOARD_BODIES)
    render(mountAt('/'))
    const section = await settledCard()

    expect(within(section).getByText('Now playing could not be loaded')).toBeInTheDocument()
    expect(within(section).getByRole('button', { name: /try again/i })).toBeInTheDocument()
    expect(within(section).queryByText(/nothing is playing/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/turned off/i)).not.toBeInTheDocument()
  })

  // The failure that must *not* reach for that panel, and the thing that goes
  // wrong when it does not.
  //
  // Three layers keep a good observation alive through a failed poll so the card
  // can say how stale it is. One dropped HTTP request is a weaker failure than
  // that chain already survives, so the observation stays — but keeping it means
  // nothing this component reads changes any more, and TanStack stops notifying
  // it. The card then sits frozen on the phrase it held when the request first
  // failed: "Last checked just now.", for ever, over an observation nobody has
  // confirmed since. That is the present-tense claim rule 1 drops the chip for,
  // with a stale clock underneath it.
  //
  // Twelve minutes and twenty-four failed polls is the shape the defect was
  // measured in. A test that failed once could not see it.
  //
  // Fails when: the `!data` guard is dropped (the observation is discarded and
  // "The Wheel" is gone), or the `errorUpdatedAt` read is dropped (the card
  // stops re-rendering and "Last checked just now." is still on screen twelve
  // minutes later).
  it('keeps the observation through failed polls, and keeps saying how old it is', async () => {
    vi.useFakeTimers()
    let attempts = 0
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        const path = new URL(url, 'http://encore.test').pathname
        if (path === '/api/nowplaying') {
          attempts += 1
          // The first check succeeds; every poll after it fails, which is the
          // shape of a server that has gone away under an open tab.
          if (attempts > 1) {
            return new Response(
              JSON.stringify({ error: { code: 'unavailable', message: 'No.' } }),
              { status: 404, headers: { 'content-type': 'application/json' } },
            )
          }
          return new Response(
            JSON.stringify(payload({ checkedAt: ago(0), observation: observation() })),
            { status: 200, headers: { 'content-type': 'application/json' } },
          )
        }
        const body = DASHBOARD_BODIES[path]
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
    render(mountAt('/'))

    const section = await settledCardWhileTicking()
    expect(within(section).getByText('Last checked just now.')).toBeInTheDocument()
    expect(attempts).toBe(1)

    // Twenty-four polls, every one of them a failure.
    for (let i = 0; i < 24; i += 1) {
      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_100)
      })
    }
    expect(attempts).toBeGreaterThan(20)

    // Still there: a dropped request is not a reason to throw away the last
    // thing Encore actually saw.
    expect(within(section).getByText('The Wheel')).toBeInTheDocument()
    expect(within(section).queryByText('Now playing could not be loaded')).not.toBeInTheDocument()

    // And no longer claiming to be fresh.
    expect(within(section).queryByText('Last checked just now.')).not.toBeInTheDocument()
    expect(within(section).getByText(/^Last checked \d+ minutes ago\.$/)).toBeInTheDocument()
  })

  // Fails when: the scope branch is dropped, or is worded as a failure — a grant
  // that never included the scope is not a check that went wrong, and offering a
  // retry for it points somebody at a button that cannot work.
  it('says the connection lacks the permission, and offers the one thing that fixes it', async () => {
    const section = await card(payload({ scopeGranted: false }))
    expect(within(section).getByText('Encore cannot see what you are playing.')).toBeInTheDocument()
    expect(
      within(section).getByText(
        'Your Spotify connection does not include permission to read your playback state. Reconnecting grants it, and nothing else in Encore is affected.',
      ),
    ).toBeInTheDocument()
    expect(within(section).getByRole('link', { name: /Reconnect Spotify/ })).toHaveAttribute(
      'href',
      '/api/auth/spotify/relink',
    )
    // Not a failure, and not something to retry.
    expect(within(section).queryByText(/failed/i)).not.toBeInTheDocument()
    expect(within(section).queryByRole('button', { name: /try again/i })).not.toBeInTheDocument()
    expect(within(section).queryByText(/nothing is playing/i)).not.toBeInTheDocument()
  })

  // The distinction the whole feature turns on. Asserted from both sides, in the
  // same test, so the two sentences cannot drift into each other.
  //
  // Fails when: a null observation is rendered with the idle copy, or the idle
  // observation is rendered with the never-checked copy — either substitution
  // trips one of the assertions below.
  it('never says nothing is playing when it simply has not looked', async () => {
    const never = await card(payload({ observation: null, checkedAt: null }))
    expect(within(never).getByText('Encore has not checked yet.')).toBeInTheDocument()
    expect(within(never).getByText('It checks every 30 seconds.')).toBeInTheDocument()
    expect(within(never).queryByText(/nothing is playing/i)).not.toBeInTheDocument()
    expect(within(never).queryByText(/last checked/i)).not.toBeInTheDocument()
  })

  // The singular the helper exists for, asserted where a reader would meet it.
  // A card that reads "It checks every 1 minutes." passes every other test here.
  //
  // Fails when: intervalPhrase is replaced by the raw number and a unit.
  it('says the interval in the singular when it is exactly one minute', async () => {
    const section = await card(payload({ observation: null, checkedAt: null, intervalSeconds: 60 }))
    expect(within(section).getByText('It checks every minute.')).toBeInTheDocument()
    expect(within(section).queryByText(/every 1 minute/)).not.toBeInTheDocument()
    expect(within(section).queryByText(/every 60 seconds/)).not.toBeInTheDocument()
  })

  // Fails when: the idle branch reuses the never-checked wording, or drops the
  // age line — a display that cannot say when it last looked cannot be trusted
  // to say a player is silent.
  it('says nothing is playing, and when it last looked', async () => {
    const section = await card(payload({ checkedAt: ago(5_000), observation: silence() }))
    expect(within(section).getByText('Nothing is playing.')).toBeInTheDocument()
    expect(within(section).getByText('Last checked just now.')).toBeInTheDocument()
    expect(within(section).queryByText(/has not checked/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/failed/i)).not.toBeInTheDocument()
  })

  // Fails when: the failure branch is merged with the never-checked one — the
  // second sentence then claims Encore has not looked, when it looked and
  // failed, which are different things to tell somebody.
  it('says a first check failed without claiming nothing is playing', async () => {
    const section = await card(
      payload({ observation: null, failed: true, checkedAt: ago(3 * 60_000) }),
    )
    expect(within(section).getByText('The last check failed 3 minutes ago.')).toBeInTheDocument()
    expect(
      within(section).getByText('Encore has not managed to see what you are playing yet.'),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/nothing is playing/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/nothing was playing/i)).not.toBeInTheDocument()
  })

  // Both ages in full, because "how stale" is the entire content of this state.
  // A card that showed the observation as current would pass a prefix match.
  //
  // Fails when: the stale branch keeps the chip — "Playing" above a four-minute
  // old observation is a present-tense claim nobody confirmed; or keeps the
  // progress figure, which is meaningless at that age and reads as a live one.
  it('says how stale a failed check has left the display, with no present tense', async () => {
    const section = await card(
      payload({
        failed: true,
        checkedAt: ago(3 * 60_000),
        observation: observation({ observedAt: ago(4 * 60_000) }),
      }),
    )
    expect(within(section).getByText('The last check failed 3 minutes ago.')).toBeInTheDocument()
    expect(
      within(section).getByText('This is what you were playing 4 minutes ago.'),
    ).toBeInTheDocument()
    expect(within(section).getByText('The Wheel')).toBeInTheDocument()
    expect(within(section).queryByText('Playing')).not.toBeInTheDocument()
    expect(within(section).queryByText('Paused')).not.toBeInTheDocument()
    expect(within(section).queryByText(/of 4:15/)).not.toBeInTheDocument()
    expect(within(section).queryByRole('meter')).not.toBeInTheDocument()
    expect(within(section).queryByText(/^Last checked /)).not.toBeInTheDocument()
  })

  // Fails when: the stale-idle case falls through to "This is what you were
  // playing", which would name a track for a player that was silent.
  it('says nothing was playing, when that is what the stale observation holds', async () => {
    const section = await card(
      payload({
        failed: true,
        checkedAt: ago(3 * 60_000),
        observation: silence({ observedAt: ago(4 * 60_000) }),
      }),
    )
    expect(within(section).getByText('The last check failed 3 minutes ago.')).toBeInTheDocument()
    expect(within(section).getByText('Nothing was playing 4 minutes ago.')).toBeInTheDocument()
    expect(within(section).queryByText(/what you were playing/i)).not.toBeInTheDocument()
  })

  // Fails when: the chip stops varying with state, or the progress figure starts
  // using formatDuration ("2m 41s") instead of formatClock ("2:41") — the second
  // is the form people recognise from a player.
  it('shows a playing track with its device and its progress as observed', async () => {
    const section = await card(payload({ checkedAt: ago(5_000), observation: observation() }))
    expect(within(section).getByText('Playing')).toBeInTheDocument()
    expect(within(section).getByRole('link', { name: 'The Wheel' })).toHaveAttribute(
      'href',
      '/tracks/spotifytrack00000001',
    )
    expect(within(section).getByText('SOHN')).toBeInTheDocument()
    expect(within(section).getByText('on Kitchen speaker')).toBeInTheDocument()
    expect(within(section).getByText('2:41 of 4:15')).toBeInTheDocument()
    expect(within(section).getByText('Last checked just now.')).toBeInTheDocument()

    const meter = within(section).getByRole('meter')
    expect(meter).toHaveAttribute('aria-label', 'Progress when Encore last checked')
    // As observed, never extrapolated: 161 of 255 seconds is 63%, and the bar
    // says the same number the figure does.
    expect(meter).toHaveAttribute('aria-valuenow', '63')
    // And a screen reader is given the figure the eye is given, not "63".
    //
    // Fails when: aria-valuetext is dropped, which every other `.meter` in
    // Encore supplies.
    expect(meter).toHaveAttribute('aria-valuetext', '2:41 of 4:15')
  })

  // Fails when: the chip is hard-coded to "Playing".
  it('says Paused when it is paused', async () => {
    const section = await card(payload({ observation: observation({ state: 'paused' }) }))
    expect(within(section).getByText('Paused')).toBeInTheDocument()
    expect(within(section).queryByText('Playing')).not.toBeInTheDocument()
  })

  // Fails when: the trackId guard is dropped and the title is always a link —
  // the assertion below then finds a link to a page that does not exist.
  it('names a track the catalogue has never seen, and does not link it', async () => {
    const section = await card(payload({ observation: observation({ trackId: '' }) }))
    expect(within(section).getByText('The Wheel')).toBeInTheDocument()
    expect(within(section).queryByRole('link', { name: 'The Wheel' })).not.toBeInTheDocument()
  })

  // Fails when: the device clause renders unconditionally — an empty deviceName
  // then produces a bare "on " with nothing after it.
  //
  // Both forms are pinned because the obvious assertion does not work: testing
  // library normalises whitespace before matching, so the rendered "on " arrives
  // here as the word "on" alone and a /^on / regex passes straight over the
  // defect. This mutation survived that assertion.
  it('renders no device clause when Spotify reported no device', async () => {
    const section = await card(payload({ observation: observation({ deviceName: '' }) }))
    expect(within(section).queryByText('on')).not.toBeInTheDocument()
    expect(within(section).queryByText(/^on\s/)).not.toBeInTheDocument()
  })

  // Fails when: the artist line renders a fallback — there is no fallback string,
  // and an absent line says the same thing without being able to be wrong.
  it('renders no second line at all when Spotify named nobody', async () => {
    const section = await card(payload({ observation: observation({ artist: '' }) }))
    expect(within(section).getByText('The Wheel')).toBeInTheDocument()
    expect(within(section).queryByText(/unknown artist/i)).not.toBeInTheDocument()
  })

  // Fails when: the progress block renders with one of the two figures missing
  // — "2:41 of —" says nothing, and a meter with no denominator cannot be drawn.
  it('renders no progress at all when there is no duration to measure it against', async () => {
    const section = await card(
      payload({ observation: observation({ durationMs: null, progressMs: 1000 }) }),
    )
    expect(within(section).queryByRole('meter')).not.toBeInTheDocument()
    expect(within(section).queryByText(/ of /)).not.toBeInTheDocument()
  })

  // Fails when: the kind note is dropped, or a podcast is rendered with the
  // track branch — a listener would then reasonably expect it in their history.
  it('names a podcast and says it will never be in the history', async () => {
    const section = await card(
      payload({
        observation: observation({
          kind: 'episode',
          title: 'The one about ducks',
          artist: 'Ducks Weekly',
          trackId: '',
        }),
      }),
    )
    expect(within(section).getByText('The one about ducks')).toBeInTheDocument()
    expect(within(section).getByText('Ducks Weekly')).toBeInTheDocument()
    expect(
      within(section).getByText('Podcasts are not part of your listening history.'),
    ).toBeInTheDocument()
    expect(
      within(section).queryByRole('link', { name: 'The one about ducks' }),
    ).not.toBeInTheDocument()
  })

  // Fails when: local files share the podcast sentence — they are not podcasts,
  // and a listener told the wrong reason cannot act on it.
  it('names a local file and says it will never be in the history', async () => {
    const section = await card(
      payload({
        observation: observation({
          kind: 'local',
          title: 'demo-2004.mp3',
          artist: 'Unreleased',
          trackId: '',
        }),
      }),
    )
    expect(within(section).getByText('demo-2004.mp3')).toBeInTheDocument()
    expect(
      within(section).getByText('Local files are not part of your listening history.'),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/Podcasts/)).not.toBeInTheDocument()
    expect(within(section).queryByRole('link', { name: 'demo-2004.mp3' })).not.toBeInTheDocument()
  })

  // Fails when: the unknown branch renders a title — an advert's own label would
  // then sit where a listener expects their music.
  it('says it cannot identify what is playing, and names nothing', async () => {
    const section = await card(payload({ observation: unidentifiable() }))
    expect(within(section).getByText('Playing')).toBeInTheDocument()
    expect(within(section).getByText('Something Encore cannot identify.')).toBeInTheDocument()
    expect(
      within(section).getByText('It will not appear in your listening history.'),
    ).toBeInTheDocument()
    expect(within(section).queryByText('The Wheel')).not.toBeInTheDocument()
  })

  // `unknown` is the only kind Encore describes rather than names, so it is the
  // only one whose line could carry a verb — and a verb there can only repeat
  // what the chip and the age sentences already say, while disagreeing with them
  // in three of the four combinations it can appear in. This pins all four at
  // once: the same line, over playing and paused, fresh and stale, with no verb
  // in any of them.
  //
  // Both contradictions this replaced are reachable. A pause during an advert
  // produces `paused` + `unknown` from one flag in the poller, which put
  // "Spotify is playing something Encore cannot identify." directly under a chip
  // reading `Paused`; and a failed check over the same observation put it under
  // "This is what you were playing 4 minutes ago."
  //
  // Fails when: any verb is reintroduced into the unidentified line.
  it.each([
    ['playing', payload({ observation: unidentifiable() })],
    ['paused', payload({ observation: unidentifiable({ state: 'paused' }) })],
    [
      'stale after playing',
      payload({
        failed: true,
        checkedAt: ago(3 * 60_000),
        observation: unidentifiable({ observedAt: ago(4 * 60_000) }),
      }),
    ],
    [
      'stale after pausing',
      payload({
        failed: true,
        checkedAt: ago(3 * 60_000),
        observation: unidentifiable({ state: 'paused', observedAt: ago(4 * 60_000) }),
      }),
    ],
  ])('describes an unidentifiable item without a verb, when %s', async (_label, np) => {
    const section = await card(np)
    expect(within(section).getByText('Something Encore cannot identify.')).toBeInTheDocument()
    // Not "is playing" over a paused item, not "was playing" over a live one,
    // and not either of them in the state whose own two sentences carry the
    // tense already.
    expect(section.textContent ?? '').not.toMatch(/(?:is|was) (?:playing|paused)[^.]*identify/)
  })

  // The chip is what carries the state over an unidentifiable item, exactly as
  // it does over a track — so it has to vary there too.
  //
  // Fails when: the unknown branch is rendered without a chip, or with a fixed
  // one: a card reading "Playing" over a paused advert is a present-tense claim
  // about something nobody is hearing.
  it('says Paused over an unidentifiable item that is paused', async () => {
    const section = await card(payload({ observation: unidentifiable({ state: 'paused' }) }))
    expect(within(section).getByText('Paused')).toBeInTheDocument()
    expect(within(section).queryByText('Playing')).not.toBeInTheDocument()
  })
})

/**
 * Every body the card can render, swept for text that is not text.
 *
 * No copy assertion can catch a literal escape sequence, because `…` in
 * bare JSX text is a valid string that compiles, renders and reads as five
 * characters on screen. Phase 3a shipped exactly that as a Critical, found by a
 * human at the last review and by none of 209 tests. This sweeps every branch
 * rather than one, because the branch nobody rendered is the one it happened in.
 *
 * Fails when: any copy in this card is written with a \uXXXX escape in JSX text,
 * or with an HTML entity where a character belongs.
 */
describe('the card, swept for text that is not text', () => {
  const EVERY_BODY: [string, NowPlaying][] = [
    ['a missing scope', payload({ scopeGranted: false })],
    ['nothing checked yet', payload({ observation: null, checkedAt: null })],
    ['a first check that failed', payload({ observation: null, failed: true })],
    ['a silent player', payload({ observation: silence() })],
    ['a stale silence', payload({ failed: true, observation: silence() })],
    ['a playing track', payload({ observation: observation() })],
    ['a paused track', payload({ observation: observation({ state: 'paused' }) })],
    [
      'a podcast',
      payload({
        observation: observation({
          kind: 'episode',
          title: 'Ducks',
          artist: 'Weekly',
          trackId: '',
        }),
      }),
    ],
    [
      'a local file',
      payload({
        observation: observation({ kind: 'local', title: 'demo.mp3', artist: '', trackId: '' }),
      }),
    ],
    ['an unidentifiable item', payload({ observation: unidentifiable() })],
    ['a paused unidentifiable item', payload({ observation: unidentifiable({ state: 'paused' }) })],
    ['a stale observation', payload({ failed: true, observation: observation() })],
    ['a stale unidentifiable item', payload({ failed: true, observation: unidentifiable() })],
  ]

  it.each(EVERY_BODY)('renders no escape or entity for %s', async (_label, np) => {
    const section = await card(np)
    const text = section.textContent ?? ''
    expect(text).not.toMatch(/\\u[0-9a-fA-F]{4}/)
    expect(text).not.toMatch(/&(?:amp|nbsp|mdash|ndash|hellip|#\d+);/)
  })
})

describe('nowPlayingPollInterval', () => {
  // Polling has to stop, and the two reasons it must are the two states whose
  // answer can never change on its own: an instance that does not poll, and an
  // account that has not granted the scope. Asserting the *number* would pass
  // for a card that polls a disabled instance for ever.
  //
  // Fails when: either guard is dropped — the corresponding case then returns a
  // number instead of false.
  it('stops for an instance that does not poll and an account that cannot be polled', () => {
    expect(nowPlayingPollInterval(payload({ enabled: false, intervalSeconds: 0 }))).toBe(false)
    expect(nowPlayingPollInterval(payload({ scopeGranted: false }))).toBe(false)
  })

  // Fails when: the floor is removed — an instance misconfigured to one second
  // would then have every open tab asking once a second.
  it('polls at the instance interval, never faster than five seconds', () => {
    expect(nowPlayingPollInterval(payload({ intervalSeconds: 30 }))).toBe(30_000)
    expect(nowPlayingPollInterval(payload({ intervalSeconds: 300 }))).toBe(300_000)
    expect(nowPlayingPollInterval(payload({ intervalSeconds: 1 }))).toBe(5_000)
  })

  // Fails when: the undefined guard is dropped and the function reads
  // .enabled off undefined, which throws inside TanStack Query's scheduler.
  it('does not schedule anything before the first answer', () => {
    expect(nowPlayingPollInterval(undefined)).toBe(false)
  })
})

describe('the now-playing poll', () => {
  function nowPlayingCalls(asked: string[]): number {
    return asked.filter((path) => path === '/api/nowplaying').length
  }

  // Stopping is the property. Asserting the interval's value would pass for a
  // card that polls a disabled instance for ever.
  //
  // Fails when: the enabled guard is dropped from nowPlayingPollInterval — the
  // count below climbs past 1.
  it('asks once and stops, on an instance that does not poll', async () => {
    vi.useFakeTimers()
    const asked = stubRoutes({
      ...DASHBOARD_BODIES,
      '/api/nowplaying': payload({ enabled: false, intervalSeconds: 0 }),
    })
    render(mountAt('/'))

    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    expect(nowPlayingCalls(asked)).toBe(1)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(10 * 60 * 1000)
    })
    expect(nowPlayingCalls(asked)).toBe(1)
  })

  // Fails when: the scopeGranted guard is dropped — an account that can never be
  // polled would have every open tab asking for ever.
  it('asks once and stops, for an account that has not granted the scope', async () => {
    vi.useFakeTimers()
    const asked = stubRoutes({
      ...DASHBOARD_BODIES,
      '/api/nowplaying': payload({ scopeGranted: false }),
    })
    render(mountAt('/'))

    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    expect(nowPlayingCalls(asked)).toBe(1)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(10 * 60 * 1000)
    })
    expect(nowPlayingCalls(asked)).toBe(1)
  })

  // Fails when: refetchInterval is removed or hard-coded to false for a healthy
  // instance — the card then never updates and shows one observation for ever.
  it('keeps asking while the instance polls and the account can be polled', async () => {
    vi.useFakeTimers()
    const asked = stubRoutes({
      ...DASHBOARD_BODIES,
      '/api/nowplaying': payload({ observation: observation() }),
    })
    render(mountAt('/'))

    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    expect(nowPlayingCalls(asked)).toBe(1)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_100)
    })
    expect(nowPlayingCalls(asked)).toBe(2)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_100)
    })
    expect(nowPlayingCalls(asked)).toBe(3)
  })
})
