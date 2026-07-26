/**
 * No such route.
 *
 * It renders inside the shell rather than as a bare page, so the navigation is
 * still there and the mistake costs one click rather than a Back button.
 */

import type { ReactElement } from 'react'
import { useLocation } from 'react-router-dom'
import { ButtonLink } from '../components/ui/Button'
import { EmptyState } from '../components/ui/EmptyState'
import { PageHeader } from '../components/ui/PageHeader'
import { Panel } from '../components/ui/Panel'

export default function NotFound(): ReactElement {
  const location = useLocation()

  return (
    <div className="space-y-5">
      <PageHeader title="Page not found" description="That address does not lead anywhere." />
      <Panel padded={false}>
        <EmptyState
          icon="search"
          title="Nothing lives here"
          description={
            <>
              <span className="tabular break-all">{location.pathname}</span> is not a page in
              Encore. It may have been renamed, or the link may have been mistyped.
            </>
          }
          action={
            <ButtonLink to="/" variant="primary">
              Back to the dashboard
            </ButtonLink>
          }
        />
      </Panel>
    </div>
  )
}
