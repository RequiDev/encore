/**
 * The router.
 *
 * Every page except the login screen is lazily imported, so the first paint
 * downloads the shell and the dashboard rather than the whole application; the
 * charts are already a separate chunk by Vite's configuration. The `Suspense`
 * boundary those lazy chunks resolve against lives in `AppShell`, which means a
 * navigation shows the navigation and a skeleton rather than a blank screen.
 *
 * The tree is deliberately three layers: providers, then the session, then the
 * shell. `RequireAuth` sits between the last two, so an expired session lands on
 * the login screen with the chrome already gone.
 */

import type { ReactElement } from 'react'
import { lazy } from 'react'
import { QueryClientProvider } from '@tanstack/react-query'
import type { RouteObject } from 'react-router-dom'
import { Navigate, Outlet, RouterProvider, createBrowserRouter } from 'react-router-dom'
import { AppShell } from './components/layout/AppShell'
import { ToastProvider } from './components/ui/Toast'
import { createQueryClient } from './lib/query'
import { RequireAdmin, RequireAuth, SessionProvider } from './lib/session'
import Login from './pages/Login'

const Dashboard = lazy(() => import('./pages/Dashboard'))
const TopTracks = lazy(() => import('./pages/TopTracks'))
const TopArtists = lazy(() => import('./pages/TopArtists'))
const TopAlbums = lazy(() => import('./pages/TopAlbums'))
const Genres = lazy(() => import('./pages/Genres'))
const History = lazy(() => import('./pages/History'))
const ArtistDetail = lazy(() => import('./pages/ArtistDetail'))
const AlbumDetail = lazy(() => import('./pages/AlbumDetail'))
const TrackDetail = lazy(() => import('./pages/TrackDetail'))
const Search = lazy(() => import('./pages/Search'))
const Sessions = lazy(() => import('./pages/Sessions'))
const Discovery = lazy(() => import('./pages/Discovery'))
const Streaks = lazy(() => import('./pages/Streaks'))
const YearInReview = lazy(() => import('./pages/YearInReview'))
const Compare = lazy(() => import('./pages/Compare'))
const Imports = lazy(() => import('./pages/Imports'))
const ImportDetail = lazy(() => import('./pages/ImportDetail'))
const Settings = lazy(() => import('./pages/Settings'))
const Admin = lazy(() => import('./pages/Admin'))
const Share = lazy(() => import('./pages/Share'))
const NotFound = lazy(() => import('./pages/NotFound'))

const queryClient = createQueryClient()

/** Providers that must wrap both the login screen and the application. */
function Root(): ReactElement {
  return (
    <SessionProvider>
      <ToastProvider>
        <Outlet />
      </ToastProvider>
    </SessionProvider>
  )
}

/** `/year` on its own means the year that is running. */
function CurrentYear(): ReactElement {
  return <Navigate to={`/year/${new Date().getFullYear()}`} replace />
}

/**
 * The route tree, exported so it can be mounted on a memory router in a test
 * without the browser history a `createBrowserRouter` insists on.
 */
export const routes: RouteObject[] = [
  {
    element: <Root />,
    children: [
      { path: '/login', element: <Login /> },
      // Outside RequireAuth and outside the shell on purpose: a visitor holding
      // a link is not a user of this instance and is shown nothing that implies
      // they are.
      { path: '/share/:token', element: <Share /> },
      {
        element: <RequireAuth />,
        children: [
          {
            element: <AppShell />,
            children: [
              { index: true, element: <Dashboard /> },
              { path: 'tracks', element: <TopTracks /> },
              { path: 'tracks/:id', element: <TrackDetail /> },
              { path: 'artists', element: <TopArtists /> },
              { path: 'artists/:id', element: <ArtistDetail /> },
              { path: 'albums', element: <TopAlbums /> },
              { path: 'albums/:id', element: <AlbumDetail /> },
              { path: 'genres', element: <Genres /> },
              { path: 'history', element: <History /> },
              { path: 'search', element: <Search /> },
              { path: 'sessions', element: <Sessions /> },
              { path: 'discovery', element: <Discovery /> },
              { path: 'streaks', element: <Streaks /> },
              { path: 'year', element: <CurrentYear /> },
              { path: 'year/:year', element: <YearInReview /> },
              { path: 'compare', element: <Compare /> },
              { path: 'compare/:userId', element: <Compare /> },
              { path: 'imports', element: <Imports /> },
              { path: 'imports/:id', element: <ImportDetail /> },
              { path: 'settings', element: <Settings /> },
              {
                path: 'settings/admin',
                element: <RequireAdmin />,
                children: [{ index: true, element: <Admin /> }],
              },
              { path: '*', element: <NotFound /> },
            ],
          },
        ],
      },
    ],
  },
]

const router = createBrowserRouter(routes)

export default function App(): ReactElement {
  return (
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}
