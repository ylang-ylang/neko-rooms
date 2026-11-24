import './public-path'
import './style.css'
import { aggregateURLs, RoomToken } from './protocol'
import { DesktopMedia } from './media'
import { RoomSession, RoomState } from './session'
import { DesktopSurface } from './desktop'

function element<T extends HTMLElement>(id: string): T {
  const value = document.getElementById(id)
  if (!value) throw new Error('Missing viewer element: ' + id)
  return value as T
}

function boot(): void {
  const form = element<HTMLFormElement>('login'), username = element<HTMLInputElement>('username'), password = element<HTMLInputElement>('password')
  const join = element<HTMLButtonElement>('join'), logout = element<HTMLButtonElement>('logout'), reconnect = element<HTMLButtonElement>('reconnect')
  const viewer = element('viewer'), desktop = element('desktop'), video = element<HTMLVideoElement>('video')
  const status = element('status'), notice = element('notice'), controlStatus = element('control-status')
  const control = element<HTMLButtonElement>('control'), audio = element<HTMLButtonElement>('audio'), fullscreen = element<HTMLButtonElement>('fullscreen')
  const volume = element<HTMLInputElement>('volume'), clipboard = element<HTMLTextAreaElement>('clipboard-text'), sendClipboard = element<HTMLButtonElement>('clipboard-send')
  const urls = aggregateURLs(location)
  let storage: Storage | undefined
  try { storage = window.sessionStorage } catch { /* Room tokens may remain RAM-only. */ }
  const token = new RoomToken(urls.base, storage)
  const roomName = location.pathname.replace(/\/+$/, '').split('/').pop() || 'Room desktop'
  let title = roomName
  try { title = decodeURIComponent(roomName) } catch { /* Keep malformed route text inert. */ }
  element('room-name').textContent = title
  document.title = title + ' · n.eko desktop'

  video.muted = true // Muted autoplay works on HTTP LAN origins; audio requires a user gesture.
  video.volume = Number(volume.value)
  const media: DesktopMedia = new DesktopMedia(video, message => session.disconnect(message, true), () => {
    audio.textContent = 'Play / unmute'
    notice.textContent = 'Playback needs a click. Press Play / unmute to start desktop audio and video.'
  })
  const session: RoomSession = new RoomSession(urls, token, {
    change: render,
    media: config => { surface.clear(); media.start(config.mime) },
    binary: bytes => media.push(bytes),
    cursor: message => surface.cursor(message),
  })
  const surface: DesktopSurface = new DesktopSurface(desktop, element<HTMLImageElement>('cursor'), session, message => { notice.textContent = message })

  function render(state: RoomState): void {
    const connected = state.phase === 'connected', busy = state.phase === 'connecting' || state.phase === 'logging-out'
    status.textContent = state.message
    status.parentElement?.classList.toggle('error', state.error)
    form.hidden = connected || state.phase === 'logging-out'
    join.disabled = username.disabled = password.disabled = busy
    reconnect.hidden = !state.hasToken || connected
    reconnect.disabled = busy
    logout.hidden = !state.hasToken
    logout.disabled = state.phase === 'logging-out'
    viewer.hidden = !connected
    control.disabled = !connected || !state.memberId
    control.textContent = session.controlled ? 'Release control' : 'Request control'
    sendClipboard.disabled = !session.controlled
    clipboard.disabled = !session.controlled
    const controller = state.members.find(value => value.id === state.controllerId)
    controlStatus.textContent = session.controlled ? 'You control the desktop' :
      state.controllerId ? 'Controlled by ' + (controller?.username || state.controllerId) : 'View only · no controller'
    if (connected && !state.memberId) controlStatus.textContent = 'View only · server did not supply your member identity'
    if (!connected) {
      media.stop()
      surface.clear()
      clipboard.value = ''
      notice.textContent = ''
    }
    surface.sync()
  }

  form.addEventListener('submit', event => {
    event.preventDefault()
    const name = username.value.trim(), secret = password.value
    if (!name || !secret) return
    password.value = '' // Never persist a room password or keep it in the form after submitting.
    void session.login(name, secret)
  })
  reconnect.addEventListener('click', () => session.reconnect())
  logout.addEventListener('click', () => { password.value = ''; clipboard.value = ''; void session.logout() })
  control.addEventListener('click', () => {
    if (session.controlled) session.control('release')
    else {
      session.control('request')
      notice.textContent = 'Control requested. Input stays disabled until the room grants control.'
    }
  })
  sendClipboard.addEventListener('click', () => {
    try { notice.textContent = session.clipboard(clipboard.value) ? 'Sent to the remote clipboard. Click the desktop and press Ctrl+V to paste.' : 'Take control before sending clipboard text.' } catch (error) {
      notice.textContent = error instanceof Error ? error.message : 'Could not send clipboard text.'
    }
  })
  audio.addEventListener('click', () => {
    video.muted = video.paused ? false : !video.muted
    // play() is called synchronously inside the user gesture, not after a permission/network await.
    void video.play().then(() => {
      if (session.state.phase !== 'connected') return
      audio.textContent = video.muted ? 'Unmute' : 'Mute'
      notice.textContent = ''
    }).catch(() => {
      if (session.state.phase === 'connected') notice.textContent = 'Playback was blocked. Click Play / unmute again or check browser audio permissions.'
    })
  })
  volume.addEventListener('input', () => { video.volume = Number(volume.value) })
  fullscreen.disabled = !document.fullscreenEnabled
  fullscreen.addEventListener('click', () => {
    const request = document.fullscreenElement ? document.exitFullscreen() : viewer.requestFullscreen()
    void request.then(() => desktop.focus({ preventScroll: true })).catch(() => { notice.textContent = 'Fullscreen is unavailable in this browser or embedding context.' })
  })
  document.addEventListener('fullscreenchange', () => { session.releaseInputs() })
  window.addEventListener('pagehide', () => { session.disconnect(); media.stop(); surface.dispose() }, { once: true })
  window.addEventListener('pageshow', event => { if (event.persisted) location.reload() }) // Rebind input after a back/forward-cache restore.

  render(session.state)
  if (token.get()) session.reconnect() // Authentication is validated again by the server before hello/media.
}

try { boot() } catch (error) {
  const status = document.getElementById('status')
  if (status) {
    status.textContent = error instanceof Error ? error.message : 'The desktop viewer could not start.'
    status.parentElement?.classList.add('error')
  }
  const form = document.getElementById('login')
  if (form) form.hidden = true
}
