/**
 * The most played albums in the range.
 *
 * The table, the pagination and the four states are `TopList`, which the track
 * and artist lists render too; this file is the configuration for one of them.
 */

import type { ReactElement } from 'react'
import { TopList } from './top/TopList'

export default function TopAlbums(): ReactElement {
  return <TopList kind="albums" />
}
