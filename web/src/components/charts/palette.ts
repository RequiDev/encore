/**
 * Where every chart's colour comes from.
 *
 * The palette lives in `index.css` as `--color-series-1 … --color-series-8`, so
 * light and dark are one definition rather than two, and a chart never hard-codes
 * a hex. This module reads those custom properties once per theme with
 * `getComputedStyle` and hands back plain strings, because Recharts writes SVG
 * `fill` and `stroke` attributes and `var()` in a presentation attribute is not
 * reliable across browsers.
 *
 * Two rules come out of running the dataviz palette validator against the
 * committed values, and both are worth writing down rather than rediscovering:
 *
 *  1. Slots are assigned in fixed order, 0, 1, 2 …, and never cycled. In daylight
 *     the fourth and fifth slots (#c04520 ember, #4d8c3a sage) collapse to ΔE 3.1
 *     under deuteranopia when they sit next to each other, so `SERIES_LIMIT` is
 *     four. A fifth series folds into "other", facets into small multiples, or
 *     picks a different form — it never invents a ninth hue.
 *  2. The teal in slot two sits a little under the chroma floor (0.083 in
 *     daylight). It still clears CVD separation at ΔE 12.1 and 3:1 against the
 *     panel, and every chart in Encore labels its series in text as well, so
 *     identity never rests on the hue alone.
 *
 * The sequential ramp for the heatmap is derived here rather than listed, so it
 * follows the lamp if the lamp ever changes. Its steps are checked by
 * `palette.test.ts` against the values the validator passed.
 */

import { useMemo } from 'react'
import { useTheme } from '../../lib/theme'
import type { ResolvedTheme } from '../../lib/theme'

/**
 * How many categorical slots a chart may seat. See the note above: this is the
 * daylight adjacency limit, not an arbitrary cap.
 */
export const SERIES_LIMIT = 4

/** How many steps the sequential ramp has. */
export const HEAT_STEPS = 5

export interface ChartPalette {
  scheme: ResolvedTheme
  /** The eight categorical slots in their fixed order. */
  series: readonly string[]
  lamp: string
  /** Sequential amber ramp, least → most. */
  heat: readonly string[]
  /** A cell or bar with nothing in it: present, but not a value. */
  empty: string
  /** Gridlines: one step off the surface, hairline, solid. */
  grid: string
  /** Axis rules and the hover cursor. */
  axis: string
  /** Axis tick text. */
  tick: string
  /** The panel a chart is drawn on, which every gap and ring is painted in. */
  surface: string
  ink: string
  muted: string
  /** The monospace stack, so axis figures match the tables beside them. */
  mono: string
  /** The text stack, for category ticks. */
  sans: string
}

/**
 * The compiled-in answer for each mode, mirroring `index.css`.
 *
 * It is the value used wherever `getComputedStyle` cannot help: jsdom under
 * test, and the first paint of a browser that has not applied the stylesheet.
 */
interface Tokens {
  series: readonly string[]
  lamp: string
  panel: string
  seam: string
  seamStrong: string
  ink: string
  inkMuted: string
}

const FALLBACK: Record<ResolvedTheme, Tokens> = {
  light: {
    series: [
      '#b8760f',
      '#237d76',
      '#5f4dc2',
      '#c04520',
      '#4d8c3a',
      '#3f68a8',
      '#b1447a',
      '#8a6a1f',
    ],
    lamp: '#b8760f',
    panel: '#fdfaf4',
    seam: '#e2d9cb',
    seamStrong: '#cabfae',
    ink: '#1e1a15',
    inkMuted: '#6b6055',
  },
  dark: {
    series: [
      '#f0a93b',
      '#3fa9a0',
      '#8b7be8',
      '#e0603a',
      '#7fbf6a',
      '#5a87c4',
      '#d66a9a',
      '#b8913f',
    ],
    lamp: '#f0a93b',
    panel: '#1b1815',
    seam: '#332d26',
    seamStrong: '#4a4137',
    ink: '#f2ede4',
    inkMuted: '#a29685',
  },
}

const MONO_FALLBACK =
  "'JetBrains Mono', 'SF Mono', 'Cascadia Mono', 'Segoe UI Mono', 'Roboto Mono', Menlo, Consolas, 'Liberation Mono', monospace"
const SANS_FALLBACK =
  "'Inter var', 'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI Variable Text', 'Segoe UI', Roboto, 'Helvetica Neue', Arial, sans-serif"

/**
 * Where the ramp starts and ends, per mode.
 *
 * The lightest step has to stay far enough from the panel to be seen at all, and
 * in daylight the lamp alone is not dark enough to give five separable steps —
 * so the dark end is carried a quarter of the way towards the ink. These two
 * pairs are what the validator passed; changing them means running it again.
 */
const RAMP: Record<ResolvedTheme, { from: number; to: number }> = {
  light: { from: 0.6, to: 0.25 },
  dark: { from: 0.4, to: 0 },
}

// --- colour arithmetic -----------------------------------------------------

type Triplet = [number, number, number]

function parseHex(value: string): Triplet | null {
  const hex = value.trim().replace('#', '')
  const full =
    hex.length === 3
      ? hex
          .split('')
          .map((c) => c + c)
          .join('')
      : hex
  if (full.length !== 6 || !/^[0-9a-f]{6}$/i.test(full)) return null
  const r = Number.parseInt(full.slice(0, 2), 16)
  const g = Number.parseInt(full.slice(2, 4), 16)
  const b = Number.parseInt(full.slice(4, 6), 16)
  return [r / 255, g / 255, b / 255]
}

const toLinear = (c: number): number => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4)
const toGamma = (c: number): number => (c <= 0.0031308 ? c * 12.92 : 1.055 * c ** (1 / 2.4) - 0.055)

/** sRGB to Oklab, the space the mixing happens in so a blend keeps its hue. */
function toOklab([r, g, b]: Triplet): Triplet {
  const R = toLinear(r)
  const G = toLinear(g)
  const B = toLinear(b)
  const l = Math.cbrt(0.4122214708 * R + 0.5363325363 * G + 0.0514459929 * B)
  const m = Math.cbrt(0.2119034982 * R + 0.6806995451 * G + 0.1073969566 * B)
  const s = Math.cbrt(0.0883024619 * R + 0.2817188376 * G + 0.6299787005 * B)
  return [
    0.2104542553 * l + 0.793617785 * m - 0.0040720468 * s,
    1.9779984951 * l - 2.428592205 * m + 0.4505937099 * s,
    0.0259040371 * l + 0.7827717662 * m - 0.808675766 * s,
  ]
}

function fromOklab([L, a, b]: Triplet): string {
  const l = (L + 0.3963377774 * a + 0.2158037573 * b) ** 3
  const m = (L - 0.1055613458 * a - 0.0638541728 * b) ** 3
  const s = (L - 0.0894841775 * a - 1.291485548 * b) ** 3
  const channels: Triplet = [
    4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s,
    -1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s,
    -0.0041960863 * l - 0.7034186147 * m + 1.707614701 * s,
  ]
  const hex = channels
    .map((value) => Math.round(Math.min(Math.max(toGamma(value), 0), 1) * 255))
    .map((value) => value.toString(16).padStart(2, '0'))
    .join('')
  return `#${hex}`
}

/** Blends two colours in Oklab. `t` of 0 is `from`, 1 is `to`. */
export function mix(from: string, to: string, t: number): string {
  const a = parseHex(from)
  const b = parseHex(to)
  if (!a || !b) return from
  const A = toOklab(a)
  const B = toOklab(b)
  const amount = Math.min(Math.max(t, 0), 1)
  return fromOklab([
    A[0] + (B[0] - A[0]) * amount,
    A[1] + (B[1] - A[1]) * amount,
    A[2] + (B[2] - A[2]) * amount,
  ])
}

function ramp(tokens: Tokens, scheme: ResolvedTheme): string[] {
  const stops = RAMP[scheme]
  const low = mix(tokens.panel, tokens.lamp, stops.from)
  const high = mix(tokens.lamp, tokens.ink, stops.to)
  return Array.from({ length: HEAT_STEPS }, (_, i) => mix(low, high, i / (HEAT_STEPS - 1)))
}

// --- reading the stylesheet ------------------------------------------------

function rootStyles(): CSSStyleDeclaration | null {
  if (typeof window === 'undefined' || typeof window.getComputedStyle !== 'function') return null
  try {
    return window.getComputedStyle(document.documentElement)
  } catch {
    return null
  }
}

function read(styles: CSSStyleDeclaration | null, name: string, fallback: string): string {
  const value = styles?.getPropertyValue(name).trim()
  return value ? value : fallback
}

/**
 * Resolves the palette for a mode. Exported for the test, which pins the ramp to
 * the steps the dataviz validator passed.
 */
export function readChartPalette(scheme: ResolvedTheme): ChartPalette {
  const fallback = FALLBACK[scheme]
  const styles = rootStyles()

  const series = fallback.series.map((value, i) => read(styles, `--color-series-${i + 1}`, value))
  const tokens: Tokens = {
    series,
    lamp: read(styles, '--color-lamp', fallback.lamp),
    panel: read(styles, '--color-panel', fallback.panel),
    seam: read(styles, '--color-seam', fallback.seam),
    seamStrong: read(styles, '--color-seam-strong', fallback.seamStrong),
    ink: read(styles, '--color-ink', fallback.ink),
    inkMuted: read(styles, '--color-ink-muted', fallback.inkMuted),
  }

  return {
    scheme,
    series,
    lamp: tokens.lamp,
    heat: ramp(tokens, scheme),
    empty: tokens.seam,
    grid: tokens.seam,
    axis: tokens.seamStrong,
    tick: tokens.inkMuted,
    surface: tokens.panel,
    ink: tokens.ink,
    muted: tokens.inkMuted,
    mono: read(styles, '--font-mono', MONO_FALLBACK),
    sans: read(styles, '--font-sans', SANS_FALLBACK),
  }
}

/**
 * The palette for the theme now on the document. It is re-read when the theme
 * changes — including when the operating system flips a "system" preference —
 * because that is the only moment the custom properties can have moved.
 */
export function useChartPalette(): ChartPalette {
  const { resolved } = useTheme()
  return useMemo(() => readChartPalette(resolved), [resolved])
}

/**
 * The colour for a series. Slots are handed out in order and clamped at
 * `SERIES_LIMIT`, so a caller that seats too many series gets a repeat it can
 * see rather than a new hue nobody can tell apart.
 */
export function seriesColor(palette: ChartPalette, slot = 0): string {
  const index = Math.min(Math.max(Math.trunc(slot), 0), SERIES_LIMIT - 1)
  return palette.series[index] ?? palette.lamp
}

/**
 * The ramp step for a value, given the largest value in the set. Zero is not a
 * step: it returns the empty colour, so "nothing happened here" never reads as
 * "a little happened here".
 */
export function heatColor(palette: ChartPalette, value: number, max: number): string {
  if (!Number.isFinite(value) || value <= 0 || !Number.isFinite(max) || max <= 0) {
    return palette.empty
  }
  const level = Math.min(Math.ceil((value / max) * HEAT_STEPS), HEAT_STEPS)
  return palette.heat[level - 1] ?? palette.empty
}
