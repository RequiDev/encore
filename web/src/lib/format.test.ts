import { describe, expect, it } from 'vitest'
import {
  formatBytes,
  formatClock,
  formatCompact,
  formatCount,
  formatDate,
  formatDateTime,
  formatDayKey,
  formatDuration,
  formatDurationShort,
  formatHour,
  formatMonth,
  formatPercent,
  formatPlural,
  formatRatio,
  formatRelative,
  formatSigned,
  formatTimeOfDay,
  formatWeekday,
  intervalPhrase,
  rankChange,
} from './format'

const MINUTE = 60_000
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR

describe('formatDuration', () => {
  it('uses days, hours and minutes for long spans', () => {
    expect(formatDuration(3 * DAY + 4 * HOUR + 12 * MINUTE)).toBe('3d 4h 12m')
  })

  it('drops the day when there is none', () => {
    expect(formatDuration(4 * HOUR + 12 * MINUTE)).toBe('4h 12m')
  })

  it('shows seconds only below an hour', () => {
    expect(formatDuration(12 * MINUTE + 30_000)).toBe('12m 30s')
    expect(formatDuration(42_000)).toBe('42s')
  })

  it('keeps a zero unit rather than skipping it', () => {
    expect(formatDuration(3 * DAY + 12 * MINUTE)).toBe('3d 0h 12m')
  })

  it('treats nonsense as nothing', () => {
    expect(formatDuration(0)).toBe('0s')
    expect(formatDuration(-5)).toBe('0s')
    expect(formatDuration(Number.NaN)).toBe('0s')
  })
})

describe('formatDurationShort', () => {
  it('picks the largest single unit', () => {
    expect(formatDurationShort(3 * DAY + 4 * HOUR)).toBe('3d')
    expect(formatDurationShort(4 * HOUR + 59 * MINUTE)).toBe('4h')
    expect(formatDurationShort(90_000)).toBe('1m')
    expect(formatDurationShort(900)).toBe('0s')
  })
})

describe('formatClock', () => {
  it('reads like a player', () => {
    expect(formatClock(3 * MINUTE + 24_000)).toBe('3:24')
    expect(formatClock(HOUR + 2 * MINUTE + 3000)).toBe('1:02:03')
    expect(formatClock(9000)).toBe('0:09')
  })
})

describe('counts', () => {
  it('separates thousands', () => {
    expect(formatCount(1204915)).toBe('1,204,915')
    expect(formatCount(0)).toBe('0')
    expect(formatCount(-42)).toBe('-42')
  })

  it('abbreviates only above a thousand', () => {
    expect(formatCompact(999)).toBe('999')
    expect(formatCompact(1200)).toBe('1.2K')
    expect(formatCompact(1_240_000)).toBe('1.2M')
  })

  it('signs a delta', () => {
    expect(formatSigned(3)).toBe('+3')
    expect(formatSigned(-2)).toBe('-2')
    expect(formatSigned(0)).toBe('0')
  })

  it('pluralises against the count', () => {
    expect(formatPlural(1, 'track')).toBe('1 track')
    expect(formatPlural(2, 'track')).toBe('2 tracks')
    expect(formatPlural(0, 'track')).toBe('0 tracks')
  })
})

describe('formatPercent', () => {
  it('drops a pointless decimal', () => {
    expect(formatPercent(0.5)).toBe('50%')
    expect(formatPercent(0.07)).toBe('7%')
  })

  it('keeps a meaningful one', () => {
    expect(formatPercent(0.125)).toBe('12.5%')
    expect(formatPercent(0.004)).toBe('0.4%')
  })

  it('never divides by zero', () => {
    expect(formatRatio(5, 0)).toBe('0%')
    expect(formatRatio(1, 4)).toBe('25%')
  })
})

describe('formatBytes', () => {
  it('steps in binary units', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(512)).toBe('512 B')
    expect(formatBytes(1024)).toBe('1.00 KB')
    expect(formatBytes(41_234_567)).toBe('39.3 MB')
  })
})

describe('rankChange', () => {
  it('calls an absent entity new rather than an infinite rise', () => {
    const change = rankChange(4, null)
    expect(change.direction).toBe('new')
    expect(change.label).toBe('NEW')
  })

  it('measures movement in places', () => {
    expect(rankChange(2, 5)).toMatchObject({ direction: 'up', places: 3, label: '+3' })
    expect(rankChange(7, 5)).toMatchObject({ direction: 'down', places: 2, label: '-2' })
    expect(rankChange(5, 5)).toMatchObject({ direction: 'flat', places: 0, label: '=' })
  })

  it('describes the movement in words for assistive technology', () => {
    expect(rankChange(2, 3).description).toBe('Up 1 place')
    expect(rankChange(9, 3).description).toBe('Down 6 places')
  })
})

describe('dates', () => {
  const instant = '2026-07-26T22:30:00Z'

  it('renders in the timezone it is given, not the browser’s', () => {
    expect(formatDate(instant, 'UTC')).toBe('26 Jul 2026')
    // Half past ten at night in London is half past seven the next morning in
    // Tokyo; the date has to move with it.
    expect(formatDate(instant, 'Asia/Tokyo')).toBe('27 Jul 2026')
  })

  it('renders a day key in the same timezone', () => {
    expect(formatDayKey(instant, 'UTC')).toBe('2026-07-26')
    expect(formatDayKey(instant, 'Asia/Tokyo')).toBe('2026-07-27')
  })

  it('renders time on a 24-hour clock', () => {
    expect(formatTimeOfDay(instant, 'UTC')).toBe('22:30')
    expect(formatDateTime(instant, 'UTC')).toContain('22:30')
  })

  it('renders month buckets', () => {
    expect(formatMonth(instant, 'UTC')).toBe('Jul 2026')
  })

  it('falls back rather than throwing on an unusable value', () => {
    expect(formatDate('not a date', 'UTC')).toBe('—')
    expect(formatDayKey('not a date', 'UTC')).toBe('')
  })
})

describe('axis labels', () => {
  it('starts the week on Monday, as the API does', () => {
    expect(formatWeekday(0)).toBe('Mon')
    expect(formatWeekday(6)).toBe('Sun')
  })

  it('pads the hour so a column of them aligns', () => {
    expect(formatHour(0)).toBe('00:00')
    expect(formatHour(8)).toBe('08:00')
    expect(formatHour(23)).toBe('23:00')
  })
})

describe('formatRelative', () => {
  const now = Date.parse('2026-07-26T12:00:00Z')

  it('collapses the immediate past', () => {
    expect(formatRelative('2026-07-26T11:59:50Z', now)).toBe('just now')
  })

  it('picks a unit that matches the distance', () => {
    expect(formatRelative('2026-07-26T11:57:00Z', now)).toBe('3 minutes ago')
    expect(formatRelative('2026-07-26T09:00:00Z', now)).toBe('3 hours ago')
    expect(formatRelative('2026-07-25T12:00:00Z', now)).toBe('yesterday')
  })

  it('handles the future too', () => {
    expect(formatRelative('2026-07-28T12:00:00Z', now)).toBe('in 2 days')
  })
})

describe('intervalPhrase', () => {
  // Every singular and plural form the now-playing copy can produce, asserted
  // in full. "It checks every 1 minutes." is the defect this exists to stop,
  // and it is invisible to a type checker and to every other test.
  //
  // Fails when: the singular branches are removed (60 then reads "1 minutes");
  // the minute branch stops requiring an exact multiple (90 then reads
  // "1.5 minutes"); or the hour branch is dropped (3600 reads "60 minutes",
  // which is true and reads like a bug).
  it.each([
    [1, 'second'],
    [15, '15 seconds'],
    [30, '30 seconds'],
    [59, '59 seconds'],
    [60, 'minute'],
    [90, '90 seconds'],
    [120, '2 minutes'],
    [300, '5 minutes'],
    [3600, 'hour'],
    [5400, '90 minutes'],
    [7200, '2 hours'],
  ])('renders %d seconds as "%s"', (seconds, want) => {
    expect(intervalPhrase(seconds)).toBe(want)
  })

  // Zero is the poller being off, and the card that would have used this is not
  // rendered at all. An empty string rather than "0 seconds" so that a stray
  // render cannot produce "every 0 seconds", which reads as a broken instance.
  //
  // Fails when: the guard is removed and the seconds branch runs on 0.
  it('renders nothing for a poller that is off', () => {
    expect(intervalPhrase(0)).toBe('')
    expect(intervalPhrase(-30)).toBe('')
  })
})
