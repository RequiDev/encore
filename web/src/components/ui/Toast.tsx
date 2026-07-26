/**
 * Transient messages.
 *
 * Toasts are for the confirmation of something the person just did — an import
 * queued, a timezone saved — not for reporting the state of the page, which
 * belongs on the page. Errors raised this way are never auto-dismissed: a
 * message that disappears before it is read is worse than no message.
 *
 * Two live regions rather than one. A failure has to interrupt whatever a screen
 * reader is saying; a success should wait its turn.
 */

import type { ReactElement, ReactNode } from 'react'
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import { Icon } from './Icon'
import type { IconName } from './Icon'

export type ToastTone = 'info' | 'success' | 'error'

export interface ToastOptions {
  title: string
  description?: string
  tone?: ToastTone
  /** Milliseconds before it fades. Errors ignore this and stay until dismissed. */
  durationMs?: number
}

interface ToastRecord extends ToastOptions {
  id: string
  tone: ToastTone
}

export interface ToastApi {
  /** Shows a toast and returns its id, so a caller can dismiss it early. */
  notify: (toast: ToastOptions) => string
  dismiss: (id: string) => void
}

const ToastContext = createContext<ToastApi | null>(null)

const DEFAULT_DURATION = 5000

const TONE_ICON: Record<ToastTone, IconName> = {
  info: 'info',
  success: 'check',
  error: 'warning',
}

const TONE_CLASS: Record<ToastTone, string> = {
  info: 'text-signal',
  success: 'text-sage',
  error: 'text-ember',
}

export function ToastProvider({ children }: { children: ReactNode }): ReactElement {
  const [toasts, setToasts] = useState<ToastRecord[]>([])
  const timers = useRef(new Map<string, number>())

  const dismiss = useCallback((id: string) => {
    const timer = timers.current.get(id)
    if (timer !== undefined) {
      window.clearTimeout(timer)
      timers.current.delete(id)
    }
    setToasts((current) => current.filter((t) => t.id !== id))
  }, [])

  const notify = useCallback(
    (options: ToastOptions): string => {
      const id =
        typeof crypto !== 'undefined' && 'randomUUID' in crypto
          ? crypto.randomUUID()
          : `t${Date.now()}${Math.random()}`
      const tone = options.tone ?? 'info'
      setToasts((current) => [...current, { ...options, id, tone }])

      if (tone !== 'error') {
        const timer = window.setTimeout(() => dismiss(id), options.durationMs ?? DEFAULT_DURATION)
        timers.current.set(id, timer)
      }
      return id
    },
    [dismiss],
  )

  useEffect(() => {
    // Captured here rather than read in the cleanup, which would be reading the
    // ref long after the component that owns it has gone.
    const pending = timers.current
    return () => {
      for (const timer of pending.values()) window.clearTimeout(timer)
      pending.clear()
    }
  }, [])

  const api = useMemo<ToastApi>(() => ({ notify, dismiss }), [notify, dismiss])

  const urgent = toasts.filter((t) => t.tone === 'error')
  const calm = toasts.filter((t) => t.tone !== 'error')
  const region = 'flex flex-col items-center gap-2 sm:items-end'

  return (
    <ToastContext.Provider value={api}>
      {children}
      <div
        // Above the mobile bottom bar, out of the way of the content on desktop.
        className="pointer-events-none fixed inset-x-0 bottom-0 z-50 flex flex-col items-center p-4 pb-20 sm:items-end lg:pb-4"
      >
        <div aria-live="assertive" aria-atomic="false" className={region}>
          {urgent.map((toast) => (
            <ToastCard key={toast.id} toast={toast} onDismiss={dismiss} />
          ))}
        </div>
        <div aria-live="polite" aria-atomic="false" className={region}>
          {calm.map((toast) => (
            <ToastCard key={toast.id} toast={toast} onDismiss={dismiss} />
          ))}
        </div>
      </div>
    </ToastContext.Provider>
  )
}

function ToastCard({
  toast,
  onDismiss,
}: {
  toast: ToastRecord
  onDismiss: (id: string) => void
}): ReactElement {
  return (
    <div className="panel-raised pointer-events-auto flex w-full max-w-sm items-start gap-3 px-3.5 py-3">
      <span className={`mt-0.5 shrink-0 ${TONE_CLASS[toast.tone]}`}>
        <Icon name={TONE_ICON[toast.tone]} />
      </span>
      <div className="min-w-0 flex-1">
        <p className="text-sm font-medium text-ink">{toast.title}</p>
        {toast.description ? (
          <p className="mt-0.5 text-xs text-ink-muted">{toast.description}</p>
        ) : null}
      </div>
      <button
        type="button"
        className="shrink-0 rounded-control p-1 text-ink-faint hover:text-ink"
        onClick={() => onDismiss(toast.id)}
        aria-label={`Dismiss: ${toast.title}`}
      >
        <Icon name="close" size={14} />
      </button>
    </div>
  )
}

const DETACHED: ToastApi = { notify: () => '', dismiss: () => undefined }

/**
 * Access to the toast queue. Falls back to a no-op outside a provider so that a
 * component rendered in isolation — a test, a page mounted on its own — does not
 * crash on a notification nobody can see anyway.
 */
export function useToast(): ToastApi {
  return useContext(ToastContext) ?? DETACHED
}
