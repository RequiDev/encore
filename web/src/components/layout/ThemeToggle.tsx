/**
 * The light/dark/system control.
 *
 * One button that cycles rather than a menu: there are three states, the icon
 * says which one is current, and cycling is one press instead of two. The change
 * is announced through a live region, because for anyone who cannot see the
 * page repaint, the button's own label changing is not an announcement.
 */

import type { ReactElement } from 'react'
import { nextThemeMode, themeLabel, useTheme } from '../../lib/theme'
import type { ThemeMode } from '../../lib/theme'
import { Icon } from '../ui/Icon'
import type { IconName } from '../ui/Icon'

const ICONS: Record<ThemeMode, IconName> = {
  light: 'sun',
  dark: 'moon',
  system: 'system',
}

export function ThemeToggle(): ReactElement {
  const { mode, setMode } = useTheme()
  const next = nextThemeMode(mode)

  return (
    <>
      <button
        type="button"
        onClick={() => setMode(next)}
        className="btn h-8 w-8 border-transparent p-0 text-ink-muted hover:text-ink"
        aria-label={`Theme: ${themeLabel(mode)}. Switch to ${themeLabel(next).toLowerCase()}.`}
        title={`Theme: ${themeLabel(mode)}`}
      >
        <Icon name={ICONS[mode]} size={16} />
      </button>
      <span aria-live="polite" className="sr-only">
        {`${themeLabel(mode)} theme`}
      </span>
    </>
  )
}
