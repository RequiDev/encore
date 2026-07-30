/**
 * The single place the client talks to the server.
 *
 * Everything goes through `request`, so CSRF handling, error shaping and JSON
 * decoding exist once rather than in every hook. Nothing here knows about React.
 */

import type { ApiErrorBody } from './types'

/** Non-HttpOnly companion to the session cookie, used for the double-submit CSRF check. */
const CSRF_COOKIE = 'encore_csrf'
const CSRF_HEADER = 'X-CSRF-Token'

const UNSAFE_METHODS = new Set(['POST', 'PUT', 'PATCH', 'DELETE'])

/**
 * A failed API call. `code` is the machine-readable identifier from the server;
 * `message` is safe to show a person, because the server never puts credentials
 * or SQL into one.
 */
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly details: Record<string, string> | undefined

  constructor(status: number, code: string, message: string, details?: Record<string, string>) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.details = details
  }

  /** True when the caller is signed out and the UI should show the login screen. */
  get isUnauthenticated(): boolean {
    return this.status === 401
  }

  get isNotFound(): boolean {
    return this.status === 404
  }
}

function readCookie(name: string): string | null {
  const prefix = `${name}=`
  for (const part of document.cookie.split('; ')) {
    if (part.startsWith(prefix)) return decodeURIComponent(part.slice(prefix.length))
  }
  return null
}

export interface RequestOptions {
  method?: string
  /** Serialised as JSON unless `formData` is given. */
  body?: unknown
  formData?: FormData
  query?: QueryValues
  signal?: AbortSignal
  /** Progress callback, only meaningful for uploads. */
  onUploadProgress?: (loaded: number, total: number) => void
}

export type QueryValues = Record<
  string,
  string | number | boolean | null | undefined | readonly string[]
>

export function buildQuery(query: QueryValues | undefined): string {
  if (!query) return ''
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    if (value === null || value === undefined || value === '') continue
    // An array becomes a repeated parameter rather than one comma-joined
    // value, because that is what Go's r.URL.Query()[key] reads back.
    if (Array.isArray(value)) {
      for (const v of value) params.append(key, String(v))
      continue
    }
    params.set(key, String(value))
  }
  const s = params.toString()
  return s ? `?${s}` : ''
}

export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const method = (options.method ?? 'GET').toUpperCase()
  const headers = new Headers({ Accept: 'application/json' })

  let body: BodyInit | undefined
  if (options.formData) {
    // Deliberately no Content-Type: the browser must set the multipart boundary.
    body = options.formData
  } else if (options.body !== undefined) {
    headers.set('Content-Type', 'application/json')
    body = JSON.stringify(options.body)
  }

  if (UNSAFE_METHODS.has(method)) {
    const token = readCookie(CSRF_COOKIE)
    if (token) headers.set(CSRF_HEADER, token)
  }

  const response = await fetch(`/api${path}${buildQuery(options.query)}`, {
    method,
    headers,
    body,
    // Session and CSRF cookies are same-origin; nginx and the Vite dev server
    // both proxy /api so this is never a cross-site request.
    credentials: 'same-origin',
    signal: options.signal ?? null,
  })

  if (response.status === 204) return undefined as T

  const contentType = response.headers.get('content-type') ?? ''
  const isJson = contentType.includes('application/json')

  if (!response.ok) {
    let code = `http_${response.status}`
    let message = response.statusText || 'Request failed'
    let details: Record<string, string> | undefined

    if (isJson) {
      try {
        const parsed = (await response.json()) as Partial<ApiErrorBody>
        if (parsed?.error) {
          code = parsed.error.code || code
          message = parsed.error.message || message
          details = parsed.error.details
        }
      } catch {
        // A body that claims to be JSON but is not tells us nothing useful;
        // keep the status-derived message rather than surfacing a parse error.
      }
    }
    throw new ApiError(response.status, code, message, details)
  }

  if (!isJson) return (await response.text()) as unknown as T
  return (await response.json()) as T
}

/**
 * Uploads with progress. `fetch` cannot report upload progress, so imports —
 * the one place where a user genuinely needs a progress bar, because the file
 * may be gigabytes — use XMLHttpRequest instead.
 */
export function upload<T>(path: string, form: FormData, options: RequestOptions = {}): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', `/api${path}${buildQuery(options.query)}`, true)
    xhr.withCredentials = true
    xhr.setRequestHeader('Accept', 'application/json')

    const token = readCookie(CSRF_COOKIE)
    if (token) xhr.setRequestHeader(CSRF_HEADER, token)

    if (options.onUploadProgress) {
      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable) options.onUploadProgress?.(e.loaded, e.total)
      }
    }

    xhr.onload = () => {
      const raw = xhr.responseText
      let parsed: unknown = null
      try {
        parsed = raw ? JSON.parse(raw) : null
      } catch {
        parsed = null
      }
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve(parsed as T)
        return
      }
      const err = (parsed as Partial<ApiErrorBody> | null)?.error
      reject(
        new ApiError(
          xhr.status,
          err?.code ?? `http_${xhr.status}`,
          err?.message ?? xhr.statusText ?? 'Upload failed',
          err?.details,
        ),
      )
    }
    xhr.onerror = () => reject(new ApiError(0, 'network', 'The upload could not reach the server.'))
    xhr.onabort = () => reject(new ApiError(0, 'aborted', 'Upload cancelled.'))

    options.signal?.addEventListener('abort', () => xhr.abort())
    xhr.send(form)
  })
}

export const api = {
  get: <T>(path: string, query?: QueryValues, signal?: AbortSignal) =>
    request<T>(path, { method: 'GET', query, signal }),
  post: <T>(path: string, body?: unknown) => request<T>(path, { method: 'POST', body }),
  patch: <T>(path: string, body?: unknown) => request<T>(path, { method: 'PATCH', body }),
  del: <T>(path: string, body?: unknown) => request<T>(path, { method: 'DELETE', body }),
  upload,
}
