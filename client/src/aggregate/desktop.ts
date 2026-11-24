import { CursorMessage, PointerInput, desktopFit, desktopPoint } from './protocol'
import { RoomSession } from './session'

// DOM coordinates stay in desktop pixels. The server alone converts keys/buttons to native X11 input.
export class DesktopSurface {
  private move: PointerInput | null = null
  private moveTimer: ReturnType<typeof setTimeout> | null = null
  private cursorURL: string | null = null
  private cursorKnown = false
  private cursorPoint: { x: number; y: number } | null = null
  private cursorSize = { width: 0, height: 0, hotspotX: 0, hotspotY: 0 }
  private resize: ResizeObserver | null = null

  constructor(private surface: HTMLElement, private image: HTMLImageElement, private session: RoomSession, private notice: (message: string) => void) {
    surface.addEventListener('pointerdown', this.down)
    surface.addEventListener('pointermove', this.moved)
    surface.addEventListener('pointerup', this.up)
    surface.addEventListener('pointercancel', this.cancel)
    surface.addEventListener('lostpointercapture', this.captureLost)
    surface.addEventListener('wheel', this.wheel, { passive: false })
    surface.addEventListener('keydown', this.key)
    surface.addEventListener('keyup', this.key)
    surface.addEventListener('contextmenu', this.context)
    surface.addEventListener('blur', this.cancel)
    window.addEventListener('blur', this.cancel)
    document.addEventListener('visibilitychange', this.visibility)
    window.addEventListener('resize', this.drawCursor)
    if (typeof ResizeObserver !== 'undefined') {
      this.resize = new ResizeObserver(this.drawCursor)
      this.resize.observe(surface)
    }
  }

  private get active(): boolean { return this.session.controlled && document.activeElement === this.surface }
  private point(event: MouseEvent, clamp = this.session.dragging): { x: number; y: number } | null {
    const size = this.session.state.desktop
    return size ? desktopPoint(event.clientX, event.clientY, this.surface.getBoundingClientRect(), size, clamp) : null
  }
  private pointer(action: PointerInput['action'], point: { x: number; y: number }, button = 0, deltaX = 0, deltaY = 0): boolean {
    this.flushMove()
    return this.session.pointer({ action, ...point, button, deltaX, deltaY })
  }
  private down = (event: PointerEvent): void => {
    if (!this.session.controlled || event.button < 0 || event.button > 4) return
    const point = this.point(event, false)
    if (!point) return // Letterbox bars never click the desktop edge.
    this.surface.focus({ preventScroll: true })
    event.preventDefault()
    if (this.pointer('down', point, event.button)) {
      this.localCursor(point)
      this.surface.setPointerCapture(event.pointerId)
    }
  }
  private moved = (event: PointerEvent): void => {
    if (!this.active) return
    const point = this.point(event)
    if (!point) return
    event.preventDefault()
    this.localCursor(point)
    this.move = { action: 'move', ...point, button: 0, deltaX: 0, deltaY: 0 }
    if (this.moveTimer === null) this.moveTimer = setTimeout(this.flushMove, 16)
  }
  private flushMove = (): void => {
    if (this.moveTimer !== null) clearTimeout(this.moveTimer)
    this.moveTimer = null
    const move = this.move
    this.move = null
    if (move && this.active) this.session.pointer(move)
  }
  private up = (event: PointerEvent): void => {
    if (!this.active) return
    const point = this.point(event, this.session.dragging)
    if (!point) return
    event.preventDefault()
    this.pointer('up', point, event.button)
    if (this.surface.hasPointerCapture(event.pointerId)) this.surface.releasePointerCapture(event.pointerId)
  }
  private captureLost = (): void => { if (this.session.dragging) this.cancel() }
  private wheel = (event: WheelEvent): void => {
    if (!this.active) return
    const point = this.point(event, false)
    if (!point) return
    event.preventDefault()
    // DOM wheel units become pixel deltas; native integer/scroll conversion belongs to the backend.
    const unit = event.deltaMode === 1 ? 16 : event.deltaMode === 2 ? this.surface.clientHeight : 1
    this.pointer('wheel', point, 0, Math.round(event.deltaX * unit), Math.round(event.deltaY * unit))
  }
  private key = (event: KeyboardEvent): void => {
    if (!this.active || event.isComposing) return
    event.preventDefault()
    event.stopPropagation()
    this.flushMove()
    this.session.key({ action: event.type === 'keydown' ? 'down' : 'up', key: event.key, code: event.code, repeat: event.repeat,
      altKey: event.altKey, ctrlKey: event.ctrlKey, shiftKey: event.shiftKey, metaKey: event.metaKey })
  }
  private context = (event: MouseEvent): void => { if (this.active) event.preventDefault() }
  private visibility = (): void => { if (document.hidden) this.cancel() }
  private cancel = (): void => {
    if (this.moveTimer !== null) clearTimeout(this.moveTimer)
    this.moveTimer = null
    this.move = null
    this.session.releaseInputs()
  }

  sync(): void {
    if (!this.session.controlled) this.cancel()
    this.surface.classList.toggle('hide-local-cursor', this.session.controlled && this.cursorKnown)
    this.drawCursor()
  }
  cursor(message: CursorMessage): void {
    if (message.action === 'position') {
      if (!this.session.controlled) this.cursorPoint = { x: message.x, y: message.y }
      this.drawCursor()
      return
    }
    this.revokeCursor()
    this.cursorSize = message
    if (!message.width || !message.height || !message.png) {
      this.cursorKnown = true // Explicit native hidden/transparent cursor.
      this.sync()
      return
    }
    const binary = atob(message.png)
    const bytes = Uint8Array.from(binary, char => char.charCodeAt(0))
    this.cursorURL = URL.createObjectURL(new Blob([bytes], { type: 'image/png' }))
    this.image.onload = () => { this.cursorKnown = true; this.sync() }
    this.image.onerror = () => { this.revokeCursor(); this.sync(); this.notice('Remote cursor could not be displayed.') }
    this.image.src = this.cursorURL
  }
  private localCursor(point: { x: number; y: number }): void {
    this.cursorPoint = point // Native cursor positions are not echoed to the current host.
    this.drawCursor()
  }
  private drawCursor = (): void => {
    const size = this.session.state.desktop
    const point = this.cursorPoint
    this.image.hidden = !size || !point || !this.cursorURL || !this.cursorKnown
    if (!size || !point || this.image.hidden) return
    const box = this.surface.getBoundingClientRect(), fit = desktopFit(box, size)
    const cursor = this.cursorSize
    this.image.style.left = (fit.left - box.left + (point.x - cursor.hotspotX) * fit.scale) + 'px'
    this.image.style.top = (fit.top - box.top + (point.y - cursor.hotspotY) * fit.scale) + 'px'
    this.image.style.width = cursor.width * fit.scale + 'px'
    this.image.style.height = cursor.height * fit.scale + 'px'
  }
  private revokeCursor(): void {
    this.image.onload = this.image.onerror = null
    this.image.removeAttribute('src')
    if (this.cursorURL) URL.revokeObjectURL(this.cursorURL)
    this.cursorURL = null
    this.cursorKnown = false
    this.image.hidden = true
  }
  clear(): void {
    this.cancel()
    this.revokeCursor()
    this.cursorPoint = null
    this.sync()
  }
  dispose(): void {
    this.clear()
    this.resize?.disconnect()
    this.surface.removeEventListener('pointerdown', this.down)
    this.surface.removeEventListener('pointermove', this.moved)
    this.surface.removeEventListener('pointerup', this.up)
    this.surface.removeEventListener('pointercancel', this.cancel)
    this.surface.removeEventListener('lostpointercapture', this.captureLost)
    this.surface.removeEventListener('wheel', this.wheel)
    this.surface.removeEventListener('keydown', this.key)
    this.surface.removeEventListener('keyup', this.key)
    this.surface.removeEventListener('contextmenu', this.context)
    this.surface.removeEventListener('blur', this.cancel)
    window.removeEventListener('blur', this.cancel)
    document.removeEventListener('visibilitychange', this.visibility)
    window.removeEventListener('resize', this.drawCursor)
  }
}
