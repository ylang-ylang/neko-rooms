// Public Manager aggregation protocol only. Native Neko auth/DC formats belong to the server.
export interface AggregateURLs {
  base: string
  login: string
  logout: string
  stream: string
}

export interface Member {
  id: string
  username: string
}

export interface DesktopSize {
  width: number
  height: number
}

export interface MediaConfig extends DesktopSize {
  mime: string
  epoch?: number
  member_id?: string
}

export interface KeyInput {
  action: 'down' | 'up'
  key: string
  code: string
  repeat: boolean
  altKey: boolean
  ctrlKey: boolean
  shiftKey: boolean
  metaKey: boolean
}

export interface PointerInput {
  action: 'move' | 'down' | 'up' | 'wheel'
  x: number
  y: number
  // DOM button numbering (0=left, 1=middle, 2=right). Backend converts to X11.
  button: number
  deltaX: number
  deltaY: number
}

export type CursorMessage =
  | { type: 'cursor'; action: 'position'; x: number; y: number }
  | { type: 'cursor'; action: 'image'; width: number; height: number; hotspotX: number; hotspotY: number; png: string }

export function aggregateURLs(location: Pick<Location, 'origin' | 'pathname'>): AggregateURLs {
  const path = location.pathname.replace(/\/+$/, '')
  if (!/\/room\/[^/]+$/.test(path)) throw new Error('Open the canonical /room/<name>/ URL to view a room.')
  const base = new URL(path + '/__aggregate', location.origin)
  if (!['http:', 'https:'].includes(base.protocol)) throw new Error('The room requires an HTTP or HTTPS origin.')
  // Never inherit the page query, hash, credentials, or any native Neko token URL.
  base.username = ''
  base.password = ''
  const stream = new URL(base.href + '/stream')
  stream.protocol = base.protocol === 'https:' ? 'wss:' : 'ws:'
  return { base: base.href, login: base.href + '/login', logout: base.href + '/logout', stream: stream.href }
}

export function record(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

export function desktopSize(value: unknown): value is DesktopSize {
  return record(value) && Number.isInteger(value.width) && Number.isInteger(value.height) &&
    Number(value.width) > 0 && Number(value.height) > 0 && Number(value.width) <= 32768 && Number(value.height) <= 32768
}

export function mediaConfig(value: unknown): value is MediaConfig {
  return desktopSize(value) && record(value) && typeof value.mime === 'string' && value.mime.length > 0 && value.mime.length <= 256 &&
    (value.member_id === undefined || typeof value.member_id === 'string') &&
    (value.epoch === undefined || (Number.isSafeInteger(value.epoch) && Number(value.epoch) >= 0))
}

export function member(value: unknown): value is Member {
  return record(value) && typeof value.id === 'string' && value.id.length > 0 && typeof value.username === 'string'
}

// Only an ephemeral room token is persisted. No password, Owner credential, or global storage key.
export class RoomToken {
  readonly key: string
  private value: string | null = null
  constructor(base: string, private storage?: Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>) {
    this.key = 'neko.aggregate.room:' + base
    try { this.value = storage?.getItem(this.key) || null } catch { /* RAM-only when storage is denied. */ }
  }
  get(): string | null { return this.value }
  set(value: string | null): void {
    this.value = value
    try {
      if (value) this.storage?.setItem(this.key, value)
      else this.storage?.removeItem(this.key)
    } catch { /* The live session still works without persistence. */ }
  }
}

export async function roomRequest(url: string, body?: { username: string; password: string }, token?: string): Promise<unknown> {
  const abort = new AbortController()
  const timer = setTimeout(() => abort.abort(), 15000)
  try {
    const headers: Record<string, string> = {}
    if (body) headers['Content-Type'] = 'application/json'
    if (token) headers.Authorization = 'Bearer ' + token
    const response = await fetch(url, {
      method: 'POST', headers, body: body ? JSON.stringify(body) : undefined,
      credentials: 'omit', redirect: 'error', cache: 'no-store', signal: abort.signal,
    })
    // Form errors are displayed in-page, never navigated to a Basic/Owner login page.
    if (response.status !== 200) {
      let message = 'Room request failed (HTTP ' + response.status + ').'
      try {
        const data: unknown = await response.json()
        if (record(data) && typeof data.message === 'string' && data.message) message += ' ' + data.message.slice(0, 500)
      } catch { /* A proxy HTML/error body is not a room protocol response. */ }
      throw new Error(message)
    }
    if (!body) return undefined // Logout may return true or an empty body.
    return await response.json()
  } catch (error) {
    if (abort.signal.aborted) throw new Error('Room request timed out. Please try again.')
    if (error instanceof Error && error.message.startsWith('Room request failed')) throw error
    throw new Error('Could not reach the room. Check the connection and try again.')
  } finally {
    clearTimeout(timer)
  }
}

export interface Box { left: number; top: number; width: number; height: number }
export function desktopFit(box: Box, size: DesktopSize): Box & { scale: number } {
  const scale = Math.min(box.width / size.width, box.height / size.height)
  const width = size.width * scale, height = size.height * scale
  return { left: box.left + (box.width - width) / 2, top: box.top + (box.height - height) / 2, width, height, scale }
}

export function desktopPoint(clientX: number, clientY: number, box: Box, size: DesktopSize, clamp = false): { x: number; y: number } | null {
  const fit = desktopFit(box, size)
  if (!(fit.scale > 0) || !Number.isFinite(clientX) || !Number.isFinite(clientY)) return null
  if (!clamp && (clientX < fit.left || clientY < fit.top || clientX >= fit.left + fit.width || clientY >= fit.top + fit.height)) return null
  const x = (clientX - fit.left) / fit.scale, y = (clientY - fit.top) / fit.scale
  return { x: Math.max(0, Math.min(size.width - 1, Math.floor(x))), y: Math.max(0, Math.min(size.height - 1, Math.floor(y))) }
}
