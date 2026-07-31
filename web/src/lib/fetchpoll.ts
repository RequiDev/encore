/**
 * The polling mechanism shared by Encore's two lazy Spotify panels: the album
 * page's never-played track list and the artist page's discography.
 *
 * Both endpoints answer immediately and fill in behind the request, so both
 * pages poll. Both have to stop, and for a reason that is not obvious from the
 * client: `pending` is unbounded server-side. Nothing in either payload says how
 * long it has held, and a claim against the fetch table that errors records
 * nothing, so the very next request lands back in `pending` for ever. Without a
 * cap, a tab left open asks the API every couple of seconds until somebody
 * closes it.
 *
 * Only the mechanism lives here. Every interval, every cap and every word of
 * copy stays on the page that renders it: the two panels wait different lengths
 * of time, because an album's walk is one request and an artist's is up to
 * forty, and they say entirely different things when they give up.
 */

/**
 * The four states both endpoints share. Pinned identical server-side by
 * `TestTheLazyFetchStatesKeepTheirWireValues`, and share one Go definition in
 * `internal/lazyfetch`.
 */
export type LazyFetchState = 'ready' | 'pending' | 'unavailable' | 'disabled'

/**
 * The next poll delay, or `false` to stop.
 *
 * `gaveUp` is the caller's cap having passed. Everything other than a running
 * fetch stops immediately, and `disabled` never polls at all because there is no
 * fetch to wait for.
 */
export function lazyPollInterval(
  state: LazyFetchState | undefined,
  gaveUp: boolean,
  everyMs: number,
): number | false {
  return state === 'pending' && !gaveUp ? everyMs : false
}

/** The `sessionStorage` key one panel uses for one entity. */
export function pollStartKey(prefix: string, id: string): string {
  return prefix + id
}

/**
 * When this tab first saw `pending` for this entity, in epoch milliseconds,
 * recording `now` the first time it is asked.
 *
 * In `sessionStorage` rather than component state on purpose: an in-memory clock
 * restarts with the component, so somebody who reloads a stuck page a few seconds
 * before the cap gets a fresh window every time and the cap never arrives.
 * Anything unreadable — private browsing, storage disabled — falls back to now,
 * which still caps the poll, just per page load.
 */
export function pollStartedAt(key: string, now: number, windowMs: number): number {
  try {
    const stored = Number(window.sessionStorage.getItem(key))
    // A start in the future is a clock that moved under us, and one older than
    // the window belongs to an earlier visit; both open a fresh window rather
    // than granting an unbounded one or refusing to try again for ever. The
    // finiteness check matters as much as the rest: Number(null) is 0 and
    // Number('x') is NaN, and treating either as a real start would either
    // restart the window on every render or report having given up before
    // anything was asked.
    if (Number.isFinite(stored) && stored > 0 && stored <= now && now - stored < windowMs) {
      return stored
    }
    window.sessionStorage.setItem(key, String(now))
  } catch {
    // See above: a per-load cap is a much smaller problem than no cap.
  }
  return now
}

/** Forgets a recorded window, so the next `pending` gets a full one. */
export function clearPollStart(key: string): void {
  try {
    window.sessionStorage.removeItem(key)
  } catch {
    // Nothing was stored, so there is nothing to clear.
  }
}
