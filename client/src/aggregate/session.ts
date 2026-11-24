import { AggregateURLs, CursorMessage, DesktopSize, KeyInput, MediaConfig, Member, PointerInput, RoomToken, mediaConfig, member, record, roomRequest } from './protocol'

export interface RoomState {
  phase: 'login' | 'connecting' | 'connected' | 'disconnected' | 'logging-out'
  message: string
  error: boolean
  memberId: string | null
  controllerId: string | null
  members: Member[]
  desktop: DesktopSize | null
  hasToken: boolean
}

interface SessionHooks {
  change: (state: RoomState) => void
  media: (config: MediaConfig) => void
  binary: (bytes: ArrayBuffer) => void
  cursor: (message: CursorMessage) => void
}

const INPUT_HIGH_WATER = 256 * 1024
const CLIPBOARD_LIMIT = 64 * 1024

export class RoomSession {
  private socket: WebSocket | null = null
  private timer: ReturnType<typeof setTimeout> | null = null
  private revision = 0
  private epoch = -1
  private keys = new Map<string, KeyInput>()
  private buttons = new Set<number>()
  private point = { x: 0, y: 0 }
  state: RoomState

  constructor(private urls: AggregateURLs, private token: RoomToken, private hooks: SessionHooks) {
    this.state = { phase: 'login', message: 'Sign in to the room.', error: false, memberId: null, controllerId: null, members: [], desktop: null, hasToken: !!token.get() }
  }

  get controlled(): boolean {
    return this.state.phase === 'connected' && !!this.state.memberId && this.state.memberId === this.state.controllerId
  }
  get dragging(): boolean { return this.buttons.size > 0 }

  private emit(): void {
    this.state = { ...this.state, hasToken: !!this.token.get() }
    this.hooks.change(this.state)
  }
  private clearTimer(): void {
    if (this.timer !== null) clearTimeout(this.timer)
    this.timer = null
  }
  private closeSocket(): void {
    this.clearTimer()
    this.releaseInputs()
    const socket = this.socket
    this.socket = null
    if (socket) {
      socket.onopen = socket.onmessage = socket.onerror = socket.onclose = null
      socket.close(1000, 'Viewer disconnected')
    }
    this.epoch = -1
  }
  private reset(message: string, error: boolean, forget = false): void {
    this.closeSocket()
    if (forget) this.token.set(null)
    this.state = { phase: this.token.get() ? 'disconnected' : 'login', message, error, memberId: null, controllerId: null, members: [], desktop: null, hasToken: !!this.token.get() }
    this.emit()
  }

  async login(username: string, password: string): Promise<void> {
    const revision = ++this.revision
    this.closeSocket()
    this.token.set(null)
    this.state = { ...this.state, phase: 'connecting', message: 'Signing in…', error: false, memberId: null, controllerId: null, desktop: null, members: [] }
    this.emit()
    try {
      const result = await roomRequest(this.urls.login, { username, password })
      if (revision !== this.revision) return
      if (!record(result) || typeof result.token !== 'string' || !result.token || result.token.length > 8192) throw new Error('The room returned an invalid login response.')
      this.token.set(result.token)
      this.open(member(result.member) ? result.member.id : null)
    } catch (error) {
      if (revision === this.revision) this.reset(error instanceof Error ? error.message : 'Room login failed.', true, true)
    }
  }

  reconnect(): void {
    ++this.revision
    this.closeSocket()
    if (!this.token.get()) {
      this.reset('Sign in to reconnect to the room.', false)
      return
    }
    this.open(null) // Server hello validates the retained token and supplies the current member identity.
  }

  private open(memberId: string | null): void {
    this.state = { phase: 'connecting', message: 'Connecting to desktop…', error: false, memberId, controllerId: null, members: [], desktop: null, hasToken: true }
    this.emit()
    let socket: WebSocket
    try {
      socket = new WebSocket(this.urls.stream)
    } catch {
      this.reset('Could not open the room connection. Try reconnecting.', true)
      return
    }
    this.socket = socket
    socket.binaryType = 'arraybuffer'
    this.timer = setTimeout(() => this.reset('The room did not start a desktop stream. Try reconnecting.', true), 15000)
    socket.onopen = () => {
      if (this.socket !== socket) return
      this.write({ type: 'auth', token: this.token.get() }) // First frame, never a URL/query credential.
    }
    socket.onmessage = event => {
      if (this.socket !== socket || this.state.phase === 'logging-out') return
      try {
        if (typeof event.data === 'string') {
          if (event.data.length > 512 * 1024) throw new Error('The room sent an oversized protocol message.')
          this.receive(JSON.parse(event.data))
        } else if (event.data instanceof ArrayBuffer) {
          if (this.state.phase !== 'connected') throw new Error('The room sent media before its configuration.')
          this.hooks.binary(event.data)
        } else throw new Error('The room sent an unsupported message format.')
      } catch (error) {
        this.reset(error instanceof Error ? error.message : 'The room sent an invalid protocol message.', true)
      }
    }
    socket.onerror = () => { if (this.socket === socket) this.reset('Room connection failed. Reconnect or sign in again.', true) }
    socket.onclose = event => {
      if (this.socket !== socket) return
      const authFailure = [1008, 4401, 4403].includes(event.code)
      this.reset(authFailure ? 'Room session ended. Please sign in again.' : 'Disconnected from the room. Reconnect to resume.', true, authFailure)
    }
  }

  private receive(message: unknown): void {
    if (!record(message) || typeof message.type !== 'string') throw new Error('The room sent an invalid protocol message.')
    if (message.type === 'error') {
      // Without a public error-code taxonomy, terminal server errors require a fresh login.
      this.reset(typeof message.message === 'string' ? message.message.slice(0, 500) : 'Room session rejected. Please sign in again.', true, true)
    } else if (message.type === 'hello' || message.type === 'media-reset') {
      const initial = message.type === 'hello'
      if (!mediaConfig(message) || (initial ? this.epoch !== -1 : this.epoch === -1 || message.epoch === undefined || message.epoch <= this.epoch)) {
        throw new Error('The room sent an invalid media configuration or epoch.')
      }
      this.releaseInputs()
      this.epoch = message.epoch ?? 0
      this.state = { ...this.state, desktop: { width: message.width, height: message.height }, memberId: message.member_id || this.state.memberId }
      const socket = this.socket
      this.hooks.media(message) // Always replace MSE/queues/URLs on a new epoch, including resize.
      if (this.socket !== socket) return // Unsupported MIME may synchronously terminate the session.
      this.clearTimer()
      this.state = { ...this.state, phase: 'connected', message: 'Connected', error: false }
      this.emit()
    } else if (message.type === 'state') {
      if (message.controller_id !== undefined && message.controller_id !== null && typeof message.controller_id !== 'string') throw new Error('The room sent an invalid controller identity.')
      if (Object.prototype.hasOwnProperty.call(message, 'controller_id')) {
        if (message.controller_id !== this.state.controllerId) this.releaseInputs()
        this.state = { ...this.state, controllerId: typeof message.controller_id === 'string' ? message.controller_id : null }
      }
      if (Array.isArray(message.members)) this.state = { ...this.state, members: message.members.filter(member) }
      this.emit() // Missing controller_id preserves the last authoritative ownership value.
    } else if (message.type === 'cursor') {
      if (message.action === 'position' && Number.isFinite(message.x) && Number.isFinite(message.y)) {
        this.hooks.cursor({ type: 'cursor', action: 'position', x: Number(message.x), y: Number(message.y) })
      } else if (message.action === 'image' && typeof message.png === 'string' && message.png.length <= 384 * 1024 &&
          [message.width, message.height, message.hotspotX, message.hotspotY].every(value => Number.isInteger(value) && Number(value) >= 0 && Number(value) <= 512)) {
        this.hooks.cursor({ type: 'cursor', action: 'image', png: message.png, width: Number(message.width), height: Number(message.height), hotspotX: Number(message.hotspotX), hotspotY: Number(message.hotspotY) })
      } else throw new Error('The room sent invalid desktop cursor data.')
    }
    // Additive future state/events do not change media or ownership by implication.
  }

  private write(message: unknown): boolean {
    if (!this.socket || this.socket.readyState !== WebSocket.OPEN) return false
    if (this.socket.bufferedAmount > INPUT_HIGH_WATER) {
      this.reset('Room input connection is congested. Reconnect to resume.', true)
      return false
    }
    try {
      this.socket.send(JSON.stringify(message))
      return true
    } catch {
      this.reset('Could not send room input. Reconnect to resume.', true)
      return false
    }
  }

  control(action: 'request' | 'release'): void {
    if (this.state.phase !== 'connected') return
    if (action === 'release') this.releaseInputs()
    if (this.write({ type: 'control', action }) && action === 'release') {
      this.state = { ...this.state, controllerId: null }
      this.emit()
    }
    // Request is never optimistic. Only an authoritative state controller_id enables input.
  }

  pointer(input: PointerInput): boolean {
    if (!this.controlled || !this.state.desktop || !Number.isFinite(input.x) || !Number.isFinite(input.y) ||
        !Number.isFinite(input.deltaX) || !Number.isFinite(input.deltaY) || !Number.isInteger(input.button) || input.button < 0 || input.button > 4) return false
    if (input.action === 'up' && !this.buttons.has(input.button)) return false
    const point = { x: Math.max(0, Math.min(this.state.desktop.width - 1, Math.round(input.x))), y: Math.max(0, Math.min(this.state.desktop.height - 1, Math.round(input.y))) }
    if (!this.write({ type: 'pointer', ...input, ...point })) return false
    this.point = point
    if (input.action === 'down') this.buttons.add(input.button)
    if (input.action === 'up') this.buttons.delete(input.button)
    return true
  }

  key(input: KeyInput): boolean {
    if (!this.controlled || !input.code || (input.action === 'up' && !this.keys.has(input.code))) return false
    if (!this.write({ type: 'key', ...input })) return false
    if (input.action === 'down') this.keys.set(input.code, input)
    else this.keys.delete(input.code)
    return true
  }

  clipboard(text: string): boolean {
    if (!this.controlled) return false
    if (new Blob([text]).size > CLIPBOARD_LIMIT) throw new Error('Clipboard text is limited to 64 KiB.')
    return this.write({ type: 'clipboard', text })
  }

  releaseInputs(): void {
    const socket = this.socket
    // Best-effort only; the private backend must reset keys/control on disconnect and enforce host at delivery.
    // Never route stale input through native WS auto-acquisition handlers.
    if (socket?.readyState === WebSocket.OPEN && this.controlled) {
      const released = [
        ...Array.from(this.keys.values(), key => ({ type: 'key', ...key, action: 'up', repeat: false, altKey: false, ctrlKey: false, shiftKey: false, metaKey: false })),
        ...Array.from(this.buttons, button => ({ type: 'pointer', action: 'up', ...this.point, button, deltaX: 0, deltaY: 0 })),
      ]
      for (const message of released) {
        try { socket.send(JSON.stringify(message)) } catch { break }
      }
    }
    this.keys.clear()
    this.buttons.clear()
  }

  disconnect(message = 'Disconnected. Reconnect to resume.', error = false): void {
    ++this.revision
    this.reset(message, error)
  }

  async logout(): Promise<void> {
    const revision = ++this.revision
    this.clearTimer()
    this.releaseInputs()
    const token = this.token.get()
    this.state = { ...this.state, phase: 'logging-out', message: 'Signing out…', error: false, controllerId: null }
    this.emit()
    let message = 'Signed out of this room.', error = false
    try { if (token) await roomRequest(this.urls.logout, undefined, token) } catch {
      message = 'Signed out locally. The server could not confirm logout; the room session may still exist.'
      error = true
    }
    if (revision === this.revision) this.reset(message, error, true)
    else if (this.token.get() === token) this.token.set(null) // A late logout must never erase a newer login.
  }
}
