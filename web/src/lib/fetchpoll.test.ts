/**
 * The mechanism behind both lazy Spotify panels' polls.
 *
 * Extracted from the album page rather than copied to the artist page: it is
 * ninety lines of pure mechanism with no copy in it, both panels need every line
 * of it, and a second copy is a second place for the cap to be got wrong. The
 * copy — which is where 2e-i's six defects were — stays on the pages, where it
 * differs entirely.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { clearPollStart, lazyPollInterval, pollStartedAt, pollStartKey } from './fetchpoll'

const WINDOW = 240_000

beforeEach(() => {
  window.sessionStorage.clear()
})

afterEach(() => {
  window.sessionStorage.clear()
})

describe('lazyPollInterval', () => {
  it('polls only while a fetch is running, and not once it has given up', () => {
    expect(lazyPollInterval('pending', false, 2000)).toBe(2000)
    expect(lazyPollInterval('pending', false, 3000)).toBe(3000)
    // The cap. Nothing on the server ends "pending", so this has to.
    expect(lazyPollInterval('pending', true, 2000)).toBe(false)
    expect(lazyPollInterval('ready', false, 2000)).toBe(false)
    expect(lazyPollInterval('unavailable', false, 2000)).toBe(false)
    // "disabled" never polls at all: there is no fetch to wait for.
    expect(lazyPollInterval('disabled', false, 2000)).toBe(false)
    expect(lazyPollInterval(undefined, false, 2000)).toBe(false)
  })
})

describe('pollStartedAt', () => {
  it('records the first instant it is asked, and returns it again afterwards', () => {
    const key = pollStartKey('encore.test.', 'thing-1')
    expect(pollStartedAt(key, 1_000_000, WINDOW)).toBe(1_000_000)
    // The second call is a re-render, or a reload: the window must not restart,
    // or a page reloaded just short of the cap never reaches it.
    expect(pollStartedAt(key, 1_050_000, WINDOW)).toBe(1_000_000)
  })

  it('opens a fresh window when the recorded start belongs to an earlier visit', () => {
    const key = pollStartKey('encore.test.', 'thing-1')
    window.sessionStorage.setItem(key, String(1_000_000))
    // Beyond the window: coming back later must mean a real attempt, not a panel
    // that reports having given up before it asked anything.
    expect(pollStartedAt(key, 1_000_000 + WINDOW + 1, WINDOW)).toBe(1_000_000 + WINDOW + 1)
  })

  it('opens a fresh window when the recorded start is in the future', () => {
    const key = pollStartKey('encore.test.', 'thing-1')
    window.sessionStorage.setItem(key, String(2_000_000))
    // A clock that moved under us. Trusting it would grant an unbounded window.
    expect(pollStartedAt(key, 1_000_000, WINDOW)).toBe(1_000_000)
  })

  it('ignores an unparseable start rather than treating it as zero', () => {
    const key = pollStartKey('encore.test.', 'thing-1')
    window.sessionStorage.setItem(key, 'not a number')
    // Number('not a number') is NaN; a Number.isFinite check is what stops it
    // becoming a start of 0, which is older than any window and would restart
    // the cap on every render — the same unbounded poll, arriving by another
    // door.
    expect(pollStartedAt(key, 1_000_000, WINDOW)).toBe(1_000_000)
    expect(window.sessionStorage.getItem(key)).toBe('1000000')
  })

  it('treats a stored zero as no start, not a real one', () => {
    const key = pollStartKey('encore.test.', 'thing-1')
    window.sessionStorage.setItem(key, '0')
    // `now - stored < windowMs` alone already rejects this whenever `now` is a
    // realistic `Date.now()` — nothing at that scale is within `windowMs` of
    // zero. A small `now`, as a very early poll in a fresh session can produce,
    // is the one case that check does not catch on its own; `stored > 0` is
    // what stops 0 being read back as a real start rather than restarting the
    // window on the very first ask.
    expect(pollStartedAt(key, 100_000, WINDOW)).toBe(100_000)
  })

  it('keys separate entities separately', () => {
    const a = pollStartKey('encore.test.', 'thing-1')
    const b = pollStartKey('encore.test.', 'thing-2')
    expect(a).not.toBe(b)
    pollStartedAt(a, 1_000_000, WINDOW)
    expect(pollStartedAt(b, 1_500_000, WINDOW)).toBe(1_500_000)
  })

  it('separates the two panels even for the same id', () => {
    // An album and an artist can share neither an id nor a stuck fetch, but the
    // prefix is what guarantees it rather than the id space happening to differ.
    const album = pollStartKey('encore.tracklist-poll-start.', 'x')
    const artist = pollStartKey('encore.discography-poll-start.', 'x')
    expect(album).not.toBe(artist)
  })
})

describe('clearPollStart', () => {
  it('forgets the window so a later pending state starts fresh', () => {
    const key = pollStartKey('encore.test.', 'thing-1')
    pollStartedAt(key, 1_000_000, WINDOW)
    clearPollStart(key)
    expect(window.sessionStorage.getItem(key)).toBeNull()
    expect(pollStartedAt(key, 1_100_000, WINDOW)).toBe(1_100_000)
  })
})

describe('a sessionStorage that throws on every access', () => {
  // Safari private mode historically threw on write rather than merely
  // refusing it, and disabled storage can throw on read too. A panel that
  // crashes because of that takes the rest of the page down with it, so every
  // accessor here is guarded and must degrade instead: a poll capped per page
  // load rather than not capped at all, which is a much smaller problem.
  beforeEach(() => {
    // Spied on the prototype, not the `window.sessionStorage` instance: jsdom's
    // Storage is not a plain object with own properties, so a spy placed there
    // is silently never called and the instance's real, non-throwing methods
    // keep answering underneath it.
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('SecurityError: private mode')
    })
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('SecurityError: private mode')
    })
    vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(() => {
      throw new Error('SecurityError: private mode')
    })
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('pollStartedAt falls back to now rather than throwing', () => {
    const key = pollStartKey('encore.test.', 'thing-1')
    expect(() => pollStartedAt(key, 1_000_000, WINDOW)).not.toThrow()
    expect(pollStartedAt(key, 1_000_000, WINDOW)).toBe(1_000_000)
  })

  it('clearPollStart does nothing rather than throwing', () => {
    const key = pollStartKey('encore.test.', 'thing-1')
    expect(() => clearPollStart(key)).not.toThrow()
  })
})
