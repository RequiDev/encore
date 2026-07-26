/**
 * The busy indicator.
 *
 * A ring rather than a bar, because Encore's async waits are of unknown length
 * and a bar implies a denominator. Where there is a real denominator — an import
 * job — the imports page draws a meter instead.
 */

import type { ReactElement } from 'react'

export interface SpinnerProps {
  /** Edge length in pixels. */
  size?: number
  /**
   * Announced to assistive technology. Give one when the spinner is the only
   * thing on screen; leave it out when it sits inside a control that already
   * says what is happening.
   */
  label?: string
  className?: string
}

export function Spinner({ size = 16, label, className }: SpinnerProps): ReactElement {
  return (
    <span
      className={['inline-flex items-center gap-2', className].filter(Boolean).join(' ')}
      role={label ? 'status' : undefined}
      aria-live={label ? 'polite' : undefined}
    >
      <svg
        width={size}
        height={size}
        viewBox="0 0 24 24"
        fill="none"
        aria-hidden="true"
        focusable="false"
        // The animation is neutralised by the reduced-motion rule in index.css,
        // which leaves a static ring rather than nothing at all.
        className="animate-spin"
      >
        <circle cx="12" cy="12" r="9" stroke="currentColor" strokeOpacity="0.25" strokeWidth="2" />
        <path
          d="M21 12a9 9 0 0 0-9-9"
          stroke="currentColor"
          strokeWidth="2"
          strokeLinecap="round"
        />
      </svg>
      {label ? <span className="eyebrow">{label}</span> : null}
    </span>
  )
}
