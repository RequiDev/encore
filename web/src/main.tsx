/**
 * Browser entry point.
 *
 * Nothing but mounting happens here. The theme has already been applied by the
 * pre-paint script in `index.html`, and every provider the application needs
 * lives in `App`, so that a test can render the whole thing without reproducing
 * a bootstrap sequence.
 */

import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App'
import './index.css'

const container = document.getElementById('root')
if (!container) {
  // The only way here is a broken index.html, and failing loudly beats a blank
  // page with nothing in the console.
  throw new Error('Encore could not start: no #root element in the document.')
}

createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
