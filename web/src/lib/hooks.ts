/**
 * Small hooks the shell and the pages both need.
 *
 * Nothing here talks to the API; these are browser-behaviour primitives, kept in
 * one place so that, for example, every dismissible surface in Encore closes on
 * the same key with the same focus behaviour.
 */

import type { RefObject } from 'react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

/** Matches the `lg` breakpoint in `index.css`; below it the sidebar is a drawer. */
export const DESKTOP_QUERY = '(min-width: 1024px)'

/**
 * Holds the most recent value in a ref, updated in an effect rather than during
 * render. It is what lets a timer or a document listener call today's callback
 * without being torn down and rebuilt every time the component re-renders.
 */
function useLatest<T>(value: T): RefObject<T> {
  const ref = useRef(value)
  useEffect(() => {
    ref.current = value
  }, [value])
  return ref
}

/**
 * Tracks a media query. Written with an effect rather than `useSyncExternalStore`
 * so that the first render is deterministic on a server-rendered or test DOM
 * where `matchMedia` may not exist at all.
 */
export function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState(false)

  useEffect(() => {
    if (typeof window.matchMedia !== 'function') return
    const list = window.matchMedia(query)
    const update = () => setMatches(list.matches)
    update()
    list.addEventListener('change', update)
    return () => list.removeEventListener('change', update)
  }, [query])

  return matches
}

/** True when the viewport is wide enough for the permanent sidebar. */
export function useIsDesktop(): boolean {
  return useMediaQuery(DESKTOP_QUERY)
}

/**
 * True when the person has asked for reduced motion. Animations are already
 * neutralised in CSS; this is for the cases only JavaScript can decide, such as
 * whether to smooth-scroll.
 */
export function usePrefersReducedMotion(): boolean {
  return useMediaQuery('(prefers-reduced-motion: reduce)')
}

/**
 * Delays a value until it stops changing. Search boxes use this so that typing
 * "radiohead" is one request rather than nine.
 */
export function useDebouncedValue<T>(value: T, delayMs = 250): T {
  const [debounced, setDebounced] = useState(value)

  useEffect(() => {
    const timer = window.setTimeout(() => setDebounced(value), delayMs)
    return () => window.clearTimeout(timer)
  }, [value, delayMs])

  return debounced
}

/**
 * Runs a callback on an interval, or not at all when `delayMs` is null. The
 * callback is held in a ref so a re-render does not restart the timer — which
 * is what makes it usable for polling a running import job.
 */
export function useInterval(callback: () => void, delayMs: number | null): void {
  const saved = useLatest(callback)

  useEffect(() => {
    if (delayMs === null) return
    const id = window.setInterval(() => saved.current(), delayMs)
    return () => window.clearInterval(id)
  }, [delayMs, saved])
}

/** Calls `handler` on Escape while `active`. */
export function useEscapeKey(active: boolean, handler: () => void): void {
  const saved = useLatest(handler)

  useEffect(() => {
    if (!active) return
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') saved.current()
    }
    document.addEventListener('keydown', onKeyDown)
    return () => document.removeEventListener('keydown', onKeyDown)
  }, [active, saved])
}

/** Calls `handler` on a pointer press outside the element while `active`. */
export function useOnClickOutside(
  ref: RefObject<HTMLElement | null>,
  active: boolean,
  handler: () => void,
): void {
  const saved = useLatest(handler)

  useEffect(() => {
    if (!active) return
    const onPointerDown = (event: PointerEvent) => {
      const node = ref.current
      if (node && event.target instanceof Node && !node.contains(event.target)) saved.current()
    }
    document.addEventListener('pointerdown', onPointerDown)
    return () => document.removeEventListener('pointerdown', onPointerDown)
  }, [ref, active, saved])
}

const FOCUSABLE = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(',')

/**
 * Keeps Tab inside an open overlay and restores focus to whatever opened it.
 *
 * Without this a keyboard user tabs straight out of an open drawer and into the
 * page behind it, which is still there and still clickable but visually covered.
 */
export function useFocusTrap(ref: RefObject<HTMLElement | null>, active: boolean): void {
  useEffect(() => {
    if (!active) return
    const container = ref.current
    if (!container) return

    const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null
    const focusables = () => Array.from(container.querySelectorAll<HTMLElement>(FOCUSABLE))

    const first = focusables()[0]
    if (first) first.focus()
    else container.focus()

    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== 'Tab') return
      const items = focusables()
      if (items.length === 0) {
        event.preventDefault()
        return
      }
      const start = items[0]
      const end = items[items.length - 1]
      if (!start || !end) return
      if (event.shiftKey && document.activeElement === start) {
        event.preventDefault()
        end.focus()
      } else if (!event.shiftKey && document.activeElement === end) {
        event.preventDefault()
        start.focus()
      }
    }

    container.addEventListener('keydown', onKeyDown)
    return () => {
      container.removeEventListener('keydown', onKeyDown)
      previous?.focus()
    }
  }, [ref, active])
}

/**
 * Sets the document title. Every page calls this, so the browser's history and
 * tab strip say which view a person was on rather than "Encore" fifteen times.
 */
export function useDocumentTitle(title: string): void {
  useEffect(() => {
    const previous = document.title
    document.title = title ? `${title} · Encore` : 'Encore'
    return () => {
      document.title = previous
    }
  }, [title])
}

/**
 * A boolean with the toggle helpers pre-bound, for the many open/closed pieces
 * of the shell. The returned object is stable, so it can be passed to a memoised
 * child without defeating the memo.
 */
export function useToggle(initial = false): {
  on: boolean
  open: () => void
  close: () => void
  toggle: () => void
  set: (value: boolean) => void
} {
  const [on, set] = useState(initial)
  const open = useCallback(() => set(true), [])
  const close = useCallback(() => set(false), [])
  const toggle = useCallback(() => set((v) => !v), [])
  return useMemo(() => ({ on, open, close, toggle, set }), [on, open, close, toggle, set])
}

/**
 * Locks background scrolling while an overlay is open, restoring whatever the
 * document had before rather than assuming it was `visible`.
 */
export function useScrollLock(active: boolean): void {
  useEffect(() => {
    if (!active) return
    const previous = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      document.body.style.overflow = previous
    }
  }, [active])
}
