/**
 * The chassis the whole application sits in.
 *
 * Three things happen here that are easy to get wrong once and then never
 * notice. The skip link is the first tab stop on the page, so a keyboard user
 * does not walk the navigation on every route. Focus moves to the main region on
 * a route change, so it does not linger on a link in a sidebar that now points
 * somewhere else. And the new page is announced through a live region, because a
 * client-side navigation is silent otherwise.
 */

import type { ReactElement } from 'react'
import { Suspense, useEffect, useRef } from 'react'
import { Outlet, useLocation } from 'react-router-dom'
import { useToggle } from '../../lib/hooks'
import { useSession } from '../../lib/session'
import { SkeletonText } from '../ui/Skeleton'
import { BottomNav, NavDrawer, Sidebar } from './Sidebar'
import { navTitleFor } from './nav'
import { ReconsentBanner } from './ReconsentBanner'
import { TopBar } from './TopBar'

export function AppShell(): ReactElement {
  const { instance } = useSession()
  const drawer = useToggle(false)
  const location = useLocation()
  const main = useRef<HTMLElement>(null)
  const firstRender = useRef(true)

  useEffect(() => {
    if (firstRender.current) {
      // On the first paint the browser has already put focus where it belongs
      // and the page title has been read; moving focus now would interrupt it.
      firstRender.current = false
      return
    }
    main.current?.focus()
  }, [location.pathname])

  // Derived rather than held in state: a live region does not announce the
  // content it was created with, only what changes afterwards, which is exactly
  // the behaviour a route change needs.
  const announcement = navTitleFor(location.pathname)
  const version = instance?.version

  return (
    <div className="flex min-h-dvh bg-chassis">
      <a className="skip-link" href="#main">
        Skip to content
      </a>

      <Sidebar version={version} />

      <div className="flex min-w-0 flex-1 flex-col">
        <TopBar />
        <ReconsentBanner />
        <main
          id="main"
          ref={main}
          // -1 so the skip link and the route change can move focus here, while
          // the region stays out of the ordinary tab sequence.
          tabIndex={-1}
          className="mx-auto w-full max-w-[1600px] flex-1 px-3 pt-5 pb-24 focus:outline-none sm:px-5 lg:pb-8"
        >
          <Suspense fallback={<PageFallback />}>
            <Outlet />
          </Suspense>
        </main>
      </div>

      <BottomNav onOpenDrawer={drawer.open} drawerOpen={drawer.on} />
      <NavDrawer open={drawer.on} onClose={drawer.close} version={version} />

      <p aria-live="polite" className="sr-only">
        {`${announcement} page`}
      </p>
    </div>
  )
}

/** Shown while a route's chunk is still downloading. */
function PageFallback(): ReactElement {
  return (
    <div role="status" aria-live="polite" aria-busy="true" className="space-y-4">
      <span className="sr-only">Loading page</span>
      <SkeletonText lines={2} className="max-w-md" />
      <div className="panel h-64" />
    </div>
  )
}
