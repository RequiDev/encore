/**
 * Buttons.
 *
 * The visual work is done by the `.btn` classes in `index.css`; this component
 * exists for the behaviour around them — the busy state that disables the
 * control while a request is in flight, and the icon-only variant that still
 * carries an accessible name.
 */

import type { ButtonHTMLAttributes, ReactElement, ReactNode } from 'react'
import { Link } from 'react-router-dom'
import type { LinkProps } from 'react-router-dom'
import { Spinner } from './Spinner'

export type ButtonVariant = 'default' | 'primary' | 'danger' | 'ghost'
export type ButtonSize = 'sm' | 'md'

const VARIANTS: Record<ButtonVariant, string> = {
  default: 'btn',
  primary: 'btn btn-primary',
  danger: 'btn btn-danger',
  ghost: 'btn border-transparent text-ink-muted hover:text-ink',
}

const SIZES: Record<ButtonSize, string> = {
  sm: 'text-xs px-2.5 py-1',
  md: '',
}

/** The class list for a button, for the rare case that needs it on another element. */
export function buttonClass(
  variant: ButtonVariant = 'default',
  size: ButtonSize = 'md',
  className?: string,
): string {
  return [VARIANTS[variant], SIZES[size], className].filter(Boolean).join(' ')
}

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant
  size?: ButtonSize
  /** Replaces the leading content with a spinner and blocks further presses. */
  busy?: boolean
}

export function Button({
  variant = 'default',
  size = 'md',
  busy = false,
  className,
  disabled,
  children,
  type = 'button',
  ...rest
}: ButtonProps): ReactElement {
  return (
    <button
      // Defaulting to "button" matters: an unmarked button inside a form submits
      // it, which is almost never what a toolbar control is for.
      type={type}
      className={buttonClass(variant, size, className)}
      disabled={disabled || busy}
      aria-busy={busy || undefined}
      {...rest}
    >
      {busy ? <Spinner size={14} /> : null}
      {children}
    </button>
  )
}

export interface IconButtonProps extends Omit<ButtonProps, 'children'> {
  /** Required: an icon alone tells a screen reader nothing. */
  label: string
  children: ReactNode
}

/** A square control holding one icon, named for assistive technology. */
export function IconButton({
  label,
  className,
  children,
  variant = 'ghost',
  ...rest
}: IconButtonProps): ReactElement {
  return (
    <Button
      variant={variant}
      aria-label={label}
      title={label}
      className={['h-8 w-8 p-0', className].filter(Boolean).join(' ')}
      {...rest}
    >
      {children}
    </Button>
  )
}

export interface ButtonLinkProps extends LinkProps {
  variant?: ButtonVariant
  size?: ButtonSize
}

/** A router link that looks like a button. Still a link, so it opens in a new tab. */
export function ButtonLink({
  variant = 'default',
  size = 'md',
  className,
  ...rest
}: ButtonLinkProps): ReactElement {
  return <Link className={buttonClass(variant, size, className)} {...rest} />
}
