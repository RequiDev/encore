/**
 * OWNED BY THE PAGES AGENT — temporary.
 *
 * The shell declares a route for every page Encore will have so that navigation,
 * lazy loading and the 404 fallback can be built and tested as a whole. Each of
 * those routes points at a module in this directory that currently renders this
 * component. Replacing one of those modules with the real page needs no change
 * to the router; when the last one is replaced, delete this file.
 */

import type { ReactElement } from 'react'
import { EmptyState } from '../components/ui/EmptyState'
import { PageHeader } from '../components/ui/PageHeader'
import { Panel } from '../components/ui/Panel'
import type { IconName } from '../components/ui/Icon'

export interface PlaceholderProps {
  title: string
  /** What this page will show once it is written. */
  summary: string
  icon?: IconName
}

export function Placeholder({
  title,
  summary,
  icon = 'dashboard',
}: PlaceholderProps): ReactElement {
  return (
    <div className="space-y-5">
      <PageHeader title={title} description={summary} />
      <Panel padded={false}>
        <EmptyState
          icon={icon}
          title="Not built yet"
          description="This view is part of the statistics work and has not been written. The route, the layout and the navigation around it are in place."
        />
      </Panel>
    </div>
  )
}
