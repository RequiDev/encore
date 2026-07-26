/**
 * The palette is the one part of a chart that cannot be checked by eye.
 *
 * These cases pin the sequential ramp to the exact steps the dataviz validator
 * passed — monotone lightness, a visible gap between steps, and a light end that
 * still clears its surface — so that a future change to the lamp, or to the way
 * the ramp is derived, fails here rather than silently shipping a scale nobody
 * can read.
 */

import { describe, expect, it } from 'vitest'
import { HEAT_STEPS, SERIES_LIMIT, heatColor, readChartPalette, seriesColor } from './palette'

describe('the chart palette', () => {
  it('derives the daylight ramp the validator passed', () => {
    expect(readChartPalette('light').heat).toEqual([
      '#d5ab79',
      '#c39763',
      '#b1834c',
      '#9f7035',
      '#8e5d1b',
    ])
  })

  it('derives the night ramp the validator passed', () => {
    expect(readChartPalette('dark').heat).toEqual([
      '#684d29',
      '#88632f',
      '#a97934',
      '#cc9138',
      '#f0a93b',
    ])
  })

  it('gives every ramp exactly as many steps as it advertises', () => {
    expect(readChartPalette('light').heat).toHaveLength(HEAT_STEPS)
    expect(readChartPalette('dark').heat).toHaveLength(HEAT_STEPS)
  })

  it('keeps nothing-played out of the ramp', () => {
    const palette = readChartPalette('dark')
    expect(heatColor(palette, 0, 100)).toBe(palette.empty)
    expect(heatColor(palette, 12, 0)).toBe(palette.empty)
    expect(heatColor(palette, 100, 100)).toBe(palette.heat[HEAT_STEPS - 1])
    expect(heatColor(palette, 1, 100)).toBe(palette.heat[0])
  })

  it('hands out series slots in order and stops at the adjacency limit', () => {
    const palette = readChartPalette('light')
    expect(seriesColor(palette, 0)).toBe(palette.series[0])
    expect(seriesColor(palette, 3)).toBe(palette.series[3])
    // A fifth series folds or facets; it never gets a hue of its own.
    expect(seriesColor(palette, 7)).toBe(palette.series[SERIES_LIMIT - 1])
  })
})
