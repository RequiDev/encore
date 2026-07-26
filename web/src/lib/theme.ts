/**
 * Light, dark and system theme selection.
 *
 * The pre-paint script in `index.html` applies the stored preference before the
 * bundle loads, so a dark-mode user never sees a white flash. This module owns
 * the same logic for the running application; the storage key must stay in step
 * with that script.
 *
 * The preference lives in a module-level store rather than in React state so the
 * top-bar toggle, the settings page and anything else asking for the theme all
 * read one value, without a provider wrapping the tree.
 */

import { useSyncExternalStore } from 'react'

/** Storage key shared with the pre-paint script in `index.html`. */
export const THEME_STORAGE_KEY = 'encore.theme'

/** What the user asked for. `system` follows the operating system. */
export type ThemeMode = 'light' | 'dark' | 'system'

/** What is actually painted, once `system` has been resolved. */
export type ResolvedTheme = 'light' | 'dark'

const DARK_QUERY = '(prefers-color-scheme: dark)'

export const THEME_MODES: readonly ThemeMode[] = ['light', 'dark', 'system']

function isThemeMode(value: unknown): value is ThemeMode {
  return value === 'light' || value === 'dark' || value === 'system'
}

/**
 * Reads the stored preference. Anything unrecognised — including a browser with
 * storage disabled, which throws rather than returning null — means "system".
 */
export function readStoredTheme(): ThemeMode {
  try {
    const stored = window.localStorage.getItem(THEME_STORAGE_KEY)
    return isThemeMode(stored) ? stored : 'system'
  } catch {
    return 'system'
  }
}

function writeStoredTheme(mode: ThemeMode): void {
  try {
    if (mode === 'system') window.localStorage.removeItem(THEME_STORAGE_KEY)
    else window.localStorage.setItem(THEME_STORAGE_KEY, mode)
  } catch {
    // Private browsing with storage disabled: the choice applies to this tab
    // only, which is better than refusing to switch at all.
  }
}

/** True when the operating system currently asks for a dark interface. */
export function systemPrefersDark(): boolean {
  return typeof window.matchMedia === 'function' && window.matchMedia(DARK_QUERY).matches
}

/** Collapses a mode to the theme that will actually be painted. */
export function resolveTheme(mode: ThemeMode): ResolvedTheme {
  if (mode === 'system') return systemPrefersDark() ? 'dark' : 'light'
  return mode
}

/**
 * Applies a mode to the document. The `dark` class is what `index.css` keys its
 * dark palette off, and `color-scheme` is what makes native controls and
 * scrollbars match.
 */
export function applyTheme(mode: ThemeMode): void {
  const resolved = resolveTheme(mode)
  const root = document.documentElement
  root.classList.toggle('dark', resolved === 'dark')
  root.style.colorScheme = resolved
}

let current: ThemeMode = 'system'
let initialised = false
const listeners = new Set<() => void>()

function emit(): void {
  for (const listener of listeners) listener()
}

function ensureInitialised(): void {
  if (initialised) return
  initialised = true
  current = readStoredTheme()
  applyTheme(current)

  // While the preference is "system" the painted theme has to follow the
  // operating system live, not only on reload.
  if (typeof window.matchMedia === 'function') {
    window.matchMedia(DARK_QUERY).addEventListener('change', () => {
      if (current === 'system') {
        applyTheme(current)
        emit()
      }
    })
  }
}

function subscribe(listener: () => void): () => void {
  ensureInitialised()
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

function getSnapshot(): ThemeMode {
  ensureInitialised()
  return current
}

/** Changes the theme, persists it, and repaints the document. */
export function setThemeMode(mode: ThemeMode): void {
  ensureInitialised()
  if (mode === current) {
    applyTheme(mode)
    return
  }
  current = mode
  writeStoredTheme(mode)
  applyTheme(mode)
  emit()
}

/** Human label for a mode, used by the toggle and by screen-reader messages. */
export function themeLabel(mode: ThemeMode): string {
  switch (mode) {
    case 'light':
      return 'Light'
    case 'dark':
      return 'Dark'
    default:
      return 'System'
  }
}

/** The next mode in the light → dark → system cycle. */
export function nextThemeMode(mode: ThemeMode): ThemeMode {
  switch (mode) {
    case 'light':
      return 'dark'
    case 'dark':
      return 'system'
    default:
      return 'light'
  }
}

/** Subscribes a component to the theme preference. */
export function useTheme(): {
  mode: ThemeMode
  resolved: ResolvedTheme
  setMode: (mode: ThemeMode) => void
} {
  const mode = useSyncExternalStore(subscribe, getSnapshot, () => 'system' as ThemeMode)
  return { mode, resolved: resolveTheme(mode), setMode: setThemeMode }
}
