/**
 * What a chart promises when nobody can see it.
 *
 * jsdom gives a `ResponsiveContainer` no size, so no SVG is drawn here — which
 * is exactly the point. Everything asserted below is the part of a chart that
 * has to work without the picture: the written summary, the empty state, the
 * keyboard grid, and the figures in the legend. If one of those regresses, the
 * chart still looks fine and is quietly unusable, and only a test catches it.
 */

import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type {
  GenreTimelinePoint,
  HeatmapCell,
  RepartitionBucket,
  TimelineBucket,
} from '../../lib/types'
import { BarChart } from './BarChart'
import { ChartCard } from './ChartCard'
import { GenreTimelineChart } from './GenreTimelineChart'
import { Heatmap } from './Heatmap'
import { HourChart } from './HourChart'
import { ShareBar } from './ShareBar'
import { Sparkline } from './Sparkline'
import { TimelineChart } from './TimelineChart'
import { WeekdayChart } from './WeekdayChart'

const DAYS: TimelineBucket[] = [
  {
    bucket: '2026-07-01T00:00:00Z',
    plays: 10,
    msPlayed: 1_800_000,
    distinctTracks: 8,
    distinctArtists: 5,
  },
  {
    bucket: '2026-07-02T00:00:00Z',
    plays: 30,
    msPlayed: 5_400_000,
    distinctTracks: 21,
    distinctArtists: 9,
  },
  {
    bucket: '2026-07-03T00:00:00Z',
    plays: 20,
    msPlayed: 3_600_000,
    distinctTracks: 14,
    distinctArtists: 7,
  },
]

const HOURS: RepartitionBucket[] = Array.from({ length: 24 }, (_, key) => ({
  key,
  plays: key,
  msPlayed: key * 60_000,
}))

const WEEK: RepartitionBucket[] = Array.from({ length: 7 }, (_, key) => ({
  key,
  plays: 10 + key,
  msPlayed: (10 + key) * 60_000,
}))

describe('TimelineChart', () => {
  it('says in words what the shape says', () => {
    render(<TimelineChart buckets={DAYS} interval="day" timeZone="UTC" metric="plays" />)

    const caption = screen.getByText(/^Listens by day/)
    expect(caption).toHaveTextContent('60 plays in total')
    expect(caption).toHaveTextContent('Busiest day: 02 Jul 2026, 30 plays')
    expect(caption).toHaveTextContent('Quietest: 01 Jul 2026, 10 plays')
    expect(caption).toHaveTextContent('The last day is 10 plays above the first')
  })

  it('describes listening time when that is what is plotted', () => {
    render(<TimelineChart buckets={DAYS} interval="day" timeZone="UTC" metric="time" />)
    expect(screen.getByText(/^Listening time by day/)).toHaveTextContent('3h 0m in total')
  })

  it('offers a way out rather than drawing empty axes', () => {
    const quiet = DAYS.map((bucket) => ({ ...bucket, plays: 0, msPlayed: 0 }))
    render(<TimelineChart buckets={quiet} interval="day" timeZone="UTC" metric="plays" />)

    expect(screen.getByText(/Nothing was played in this range/)).toBeInTheDocument()
    expect(screen.queryByText(/in total/)).not.toBeInTheDocument()
  })
})

describe('the repartitions', () => {
  it('names the busiest hour and its share', () => {
    render(<HourChart buckets={HOURS} />)
    const caption = screen.getByText(/^Listens by hour of day/)
    expect(caption).toHaveTextContent('Busiest hour: 23:00, 23 plays')
    expect(caption).toHaveTextContent('Quietest: 00:00, 0 plays')
  })

  it('names weekdays in full, not as axis abbreviations', () => {
    render(<WeekdayChart buckets={WEEK} />)
    expect(screen.getByText(/^Listens by day of the week/)).toHaveTextContent(
      'Busiest day: Sunday, 16 plays',
    )
  })

  it('degrades to an empty state when the range is silent', () => {
    render(<WeekdayChart buckets={WEEK.map((bucket) => ({ ...bucket, plays: 0, msPlayed: 0 }))} />)
    expect(screen.getByText(/no pattern to show yet/)).toBeInTheDocument()
  })
})

describe('BarChart', () => {
  it('summarises the ranking it draws', () => {
    render(
      <BarChart
        label="Top artists by plays"
        data={[
          { key: 'ar1', label: 'Radiohead', value: 120 },
          { key: 'ar2', label: 'Portishead', value: 80 },
          { key: 'ar3', label: 'Massive Attack', value: 40 },
        ]}
      />,
    )
    const caption = screen.getByText(/^Top artists by plays/)
    expect(caption).toHaveTextContent('led by Radiohead with 120')
    expect(caption).toHaveTextContent('down to Massive Attack with 40')
  })

  it('says so when there is nothing to rank', () => {
    render(<BarChart label="Top artists by plays" data={[]} />)
    expect(screen.getByText(/nothing to rank in this range/)).toBeInTheDocument()
  })
})

describe('GenreTimelineChart', () => {
  it('gives fewer than five genres one band each, with no "Other"', () => {
    const points: GenreTimelinePoint[] = [
      { bucket: '2026-01-01T00:00:00Z', genre: 'dream pop', plays: 10, msPlayed: 0 },
      { bucket: '2026-01-01T00:00:00Z', genre: 'synthwave', plays: 6, msPlayed: 0 },
      { bucket: '2026-01-01T00:00:00Z', genre: 'ambient', plays: 2, msPlayed: 0 },
    ]

    const { container } = render(
      <GenreTimelineChart
        points={points}
        genres={['dream pop', 'synthwave', 'ambient']}
        interval="month"
        timeZone="UTC"
        metric="plays"
      />,
    )

    expect(screen.queryByText('Other genres')).not.toBeInTheDocument()
    expect(container.querySelectorAll('li')).toHaveLength(3)
    const caption = screen.getByText(/^Genre listening by month/)
    expect(caption).toHaveTextContent('across 3 genres')
    expect(caption).toHaveTextContent('led by dream pop with 10')
  })

  it('folds rank five and beyond into one "Other" band, summed correctly', () => {
    // Ranked, most-played first — as the server's top-eight cap always hands
    // the component. The last two are given the largest values on purpose, so
    // a wrong fold (e.g. dropping the overflow, or summing the wrong series)
    // shows up as the wrong leader or the wrong total, not merely a missing row.
    const genres = ['a', 'b', 'c', 'd', 'e', 'f']
    const points: GenreTimelinePoint[] = [
      { bucket: '2026-01-01T00:00:00Z', genre: 'a', plays: 10, msPlayed: 0 },
      { bucket: '2026-01-01T00:00:00Z', genre: 'b', plays: 8, msPlayed: 0 },
      { bucket: '2026-01-01T00:00:00Z', genre: 'c', plays: 6, msPlayed: 0 },
      { bucket: '2026-01-01T00:00:00Z', genre: 'd', plays: 4, msPlayed: 0 },
      { bucket: '2026-01-01T00:00:00Z', genre: 'e', plays: 20, msPlayed: 0 },
      { bucket: '2026-01-01T00:00:00Z', genre: 'f', plays: 5, msPlayed: 0 },
    ]

    const { container } = render(
      <GenreTimelineChart
        points={points}
        genres={genres}
        interval="month"
        timeZone="UTC"
        metric="plays"
      />,
    )

    // Four named bands (SERIES_LIMIT) plus one "Other" — never a sixth hue.
    expect(container.querySelectorAll('li')).toHaveLength(5)
    expect(screen.getByText('Other genres')).toBeInTheDocument()

    const caption = screen.getByText(/^Genre listening by month/)
    // Grand total is all six genres' plays (10+8+6+4+20+5 = 53); the leader is
    // "Other", whose value is exactly the folded pair's sum (20+5 = 25) rather
    // than either overflow genre alone or the whole grand total.
    expect(caption).toHaveTextContent('53 across 5 genres')
    expect(caption).toHaveTextContent('led by Other genres with 25')
  })

  it('does not crash on an empty points array', () => {
    render(
      <GenreTimelineChart
        points={[]}
        genres={['dream pop', 'synthwave']}
        interval="month"
        timeZone="UTC"
        metric="plays"
      />,
    )
    expect(
      screen.getByText(/Nothing was played in this range, so there is no genre trend to show yet/),
    ).toBeInTheDocument()
  })
})

describe('Heatmap', () => {
  const cells: HeatmapCell[] = [
    { weekday: 0, hour: 0, plays: 5, msPlayed: 60_000 },
    { weekday: 5, hour: 22, plays: 40, msPlayed: 900_000 },
  ]

  it('is a real table, so the values are readable without the colours', () => {
    render(<Heatmap cells={cells} />)

    const table = screen.getByRole('table')
    expect(within(table).getByText(/Busiest: Saturday at 22:00, 40 plays/)).toBeInTheDocument()
    // Seven weekday rows, each with its own row header.
    expect(within(table).getAllByRole('rowheader')).toHaveLength(7)
    expect(within(table).getAllByRole('button')).toHaveLength(7 * 24)
    expect(within(table).getByRole('button', { name: '40 plays, 15m 0s' })).toBeInTheDocument()
  })

  it('is one tab stop that the arrow keys walk', () => {
    render(<Heatmap cells={cells} />)

    const cellButtons = screen.getAllByRole('button')
    const tabbable = cellButtons.filter((cell) => cell.tabIndex === 0)
    expect(tabbable).toHaveLength(1)

    const first = tabbable[0]
    expect(first).toBeDefined()
    if (!first) return

    act(() => first.focus())
    fireEvent.keyDown(first, { key: 'ArrowRight' })
    expect(document.activeElement).toHaveAttribute('data-cell', '0-1')

    fireEvent.keyDown(document.activeElement as HTMLElement, { key: 'ArrowDown' })
    expect(document.activeElement).toHaveAttribute('data-cell', '1-1')

    fireEvent.keyDown(document.activeElement as HTMLElement, { key: 'End' })
    expect(document.activeElement).toHaveAttribute('data-cell', '1-23')

    // The edges hold rather than wrapping to another day.
    fireEvent.keyDown(document.activeElement as HTMLElement, { key: 'ArrowRight' })
    expect(document.activeElement).toHaveAttribute('data-cell', '1-23')
  })

  it('reads out the cell under the pointer', () => {
    render(<Heatmap cells={cells} />)
    expect(screen.getByText(/Point at a cell/)).toBeInTheDocument()

    fireEvent.mouseEnter(screen.getByRole('button', { name: '40 plays, 15m 0s' }))
    expect(screen.getByText('Saturday 22:00')).toBeInTheDocument()
  })

  it('shows nothing rather than a grid of empty cells', () => {
    render(<Heatmap cells={[]} />)
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
    expect(screen.getByText(/no weekly pattern yet/)).toBeInTheDocument()
  })
})

describe('Sparkline', () => {
  it('carries its trend as an accessible name', () => {
    render(<Sparkline values={[4, 9, 2, 12]} label="Listens per day" />)
    const image = screen.getByRole('img')
    expect(image).toHaveAccessibleName(
      'Listens per day: 4 points, up from 4 to 12. Highest 12, lowest 2.',
    )
  })

  it('draws nothing from a single point, which is not a trend', () => {
    const { container } = render(<Sparkline values={[4]} label="Listens per day" />)
    expect(container).toBeEmptyDOMElement()
  })
})

describe('ShareBar', () => {
  it('writes the percentage out beside the bar', () => {
    render(<ShareBar value={25} total={100} label="Radiohead — share of your listening time" />)
    const meter = screen.getByRole('meter')
    expect(meter).toHaveAttribute('aria-valuenow', '25')
    expect(meter).toHaveAttribute('aria-valuetext', '25%')
    expect(screen.getByText('25%')).toBeInTheDocument()
  })

  it('treats an empty whole as no share rather than as an error', () => {
    render(<ShareBar value={25} total={0} label="Share" />)
    expect(screen.getByRole('meter')).toHaveAttribute('aria-valuenow', '0')
    expect(screen.getByText('0%')).toBeInTheDocument()
  })
})

describe('ChartCard', () => {
  it('gives a chart a heading below the page title, and a home for its control', () => {
    render(
      <ChartCard title="Hour of day" description="When you listen" control={<button>Plays</button>}>
        <p>the plot</p>
      </ChartCard>,
    )

    expect(screen.getByRole('heading', { level: 2, name: 'Hour of day' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Plays' })).toBeInTheDocument()
    expect(screen.getByText('the plot')).toBeInTheDocument()
  })
})
