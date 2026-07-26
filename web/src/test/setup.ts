/**
 * Vitest environment setup, loaded before every test file.
 *
 * jsdom implements neither `matchMedia` nor `ResizeObserver`, and both are used
 * unconditionally — by the theme and the responsive layout in the first case,
 * by Recharts in the second. Stubbing them here rather than in each test keeps
 * the failure mode honest: a component that genuinely misuses them still fails,
 * it just does not fail with "not a function".
 */

import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach, beforeAll, vi } from 'vitest'

beforeAll(() => {
  if (typeof window.matchMedia !== 'function') {
    Object.defineProperty(window, 'matchMedia', {
      writable: true,
      value: (query: string): MediaQueryList =>
        ({
          matches: false,
          media: query,
          onchange: null,
          addEventListener: () => undefined,
          removeEventListener: () => undefined,
          addListener: () => undefined,
          removeListener: () => undefined,
          dispatchEvent: () => false,
        }) as unknown as MediaQueryList,
    })
  }

  if (typeof globalThis.ResizeObserver === 'undefined') {
    globalThis.ResizeObserver = class {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    } as unknown as typeof ResizeObserver
  }
})

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
})
