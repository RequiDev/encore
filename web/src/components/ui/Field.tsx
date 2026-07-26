/**
 * Form fields.
 *
 * The label, the hint and the error are wired to the control automatically
 * through context rather than by each caller remembering to pass an id and an
 * `aria-describedby`. Labelling is a floor, not a feature, and the surest way to
 * keep a floor is to make it impossible to step off.
 */

import type {
  InputHTMLAttributes,
  ReactElement,
  ReactNode,
  SelectHTMLAttributes,
  TextareaHTMLAttributes,
} from 'react'
import { createContext, useContext, useId } from 'react'

interface FieldWiring {
  id: string
  describedBy: string | undefined
  invalid: boolean
}

const FieldContext = createContext<FieldWiring | null>(null)

function useWiring(): FieldWiring | null {
  return useContext(FieldContext)
}

export interface FieldProps {
  label: ReactNode
  /** Guidance shown under the control, and announced with it. */
  hint?: ReactNode
  /** A validation message. Its presence marks the control invalid. */
  error?: ReactNode
  /** Hides the label visually while keeping it for assistive technology. */
  labelHidden?: boolean
  className?: string
  children: ReactNode
}

export function Field({
  label,
  hint,
  error,
  labelHidden = false,
  className,
  children,
}: FieldProps): ReactElement {
  const id = useId()
  const hintId = hint ? `${id}-hint` : undefined
  const errorId = error ? `${id}-error` : undefined
  const describedBy = [hintId, errorId].filter(Boolean).join(' ') || undefined

  return (
    <FieldContext.Provider value={{ id, describedBy, invalid: Boolean(error) }}>
      <div className={['min-w-0', className].filter(Boolean).join(' ')}>
        <label htmlFor={id} className={labelHidden ? 'sr-only' : 'eyebrow block'}>
          {label}
        </label>
        <div className={labelHidden ? '' : 'mt-1.5'}>{children}</div>
        {hint ? (
          <p id={hintId} className="mt-1.5 text-xs text-ink-faint">
            {hint}
          </p>
        ) : null}
        {error ? (
          <p id={errorId} className="mt-1.5 text-xs text-ember" role="alert">
            {error}
          </p>
        ) : null}
      </div>
    </FieldContext.Provider>
  )
}

export type InputProps = InputHTMLAttributes<HTMLInputElement>

export function Input({ className, ...rest }: InputProps): ReactElement {
  const wiring = useWiring()
  return (
    <input
      id={wiring?.id}
      aria-describedby={wiring?.describedBy}
      aria-invalid={wiring?.invalid || undefined}
      className={['field', className].filter(Boolean).join(' ')}
      {...rest}
    />
  )
}

export type SelectProps = SelectHTMLAttributes<HTMLSelectElement>

export function Select({ className, children, ...rest }: SelectProps): ReactElement {
  const wiring = useWiring()
  return (
    <select
      id={wiring?.id}
      aria-describedby={wiring?.describedBy}
      aria-invalid={wiring?.invalid || undefined}
      className={['field', className].filter(Boolean).join(' ')}
      {...rest}
    >
      {children}
    </select>
  )
}

export type TextareaProps = TextareaHTMLAttributes<HTMLTextAreaElement>

export function Textarea({ className, ...rest }: TextareaProps): ReactElement {
  const wiring = useWiring()
  return (
    <textarea
      id={wiring?.id}
      aria-describedby={wiring?.describedBy}
      aria-invalid={wiring?.invalid || undefined}
      className={['field', className].filter(Boolean).join(' ')}
      {...rest}
    />
  )
}

export interface CheckboxProps extends Omit<InputHTMLAttributes<HTMLInputElement>, 'type'> {
  label: ReactNode
  hint?: ReactNode
}

/**
 * A checkbox with its label beside it rather than above, which is the one place
 * the `Field` layout does not apply.
 */
export function Checkbox({ label, hint, className, ...rest }: CheckboxProps): ReactElement {
  const id = useId()
  const hintId = hint ? `${id}-hint` : undefined
  return (
    <div className={['flex items-start gap-2.5', className].filter(Boolean).join(' ')}>
      <input
        id={id}
        type="checkbox"
        aria-describedby={hintId}
        className="mt-0.5 h-4 w-4 accent-lamp"
        {...rest}
      />
      <div className="min-w-0">
        <label htmlFor={id} className="text-sm text-ink">
          {label}
        </label>
        {hint ? (
          <p id={hintId} className="mt-0.5 text-xs text-ink-faint">
            {hint}
          </p>
        ) : null}
      </div>
    </div>
  )
}
