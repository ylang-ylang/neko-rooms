const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const { createRequire } = require('node:module')
const { dirname, resolve } = require('node:path')
const { test } = require('node:test')
const { runInNewContext } = require('node:vm')
const ts = require('typescript')

const root = resolve(__dirname, '..')
const config = ts.readConfigFile(resolve(root, 'tsconfig.json'), ts.sys.readFile).config
const options = ts.convertCompilerOptionsFromJson(config.compilerOptions, root).options
const plain = value => JSON.parse(JSON.stringify(value))
const key = (action = 'down', overrides = {}) => ({ action, key: 'Control', code: 'ControlLeft', repeat: false, altKey: false, ctrlKey: action === 'down', shiftKey: false, metaKey: false, ...overrides })
const pointer = (action = 'down', overrides = {}) => ({ action, x: 100, y: 200, button: 0, deltaX: 0, deltaY: 0, ...overrides })
const hello = { type: 'hello', mime: 'video/webm; codecs="vp8,opus"', width: 1920, height: 1080, member_id: 'me' }

// The repository's native CJS/installed TS compiler pattern. These mocks test plumbing, NOT media decoding.
function loader(globals = {}, mocks = {}) {
  const cache = new Map()
  function load(file) {
    const filename = resolve(root, file)
    if (cache.has(filename)) return cache.get(filename)
    const source = readFileSync(filename, 'utf8')
    const code = ts.transpileModule(source, { compilerOptions: { ...options, module: ts.ModuleKind.CommonJS }, fileName: filename }).outputText
    const exports = {}, localRequire = createRequire(filename)
    cache.set(filename, exports)
    runInNewContext(code, {
      exports, Error, console, URL, Blob, ArrayBuffer, Uint8Array, AbortController, atob, setTimeout, clearTimeout,
      require: name => Object.prototype.hasOwnProperty.call(mocks, name) ? mocks[name] :
        name.startsWith('.') ? load(resolve(dirname(filename), name + '.ts')) : localRequire(name),
      ...globals,
    }, { filename })
    return exports
  }
  return load
}

function timers() {
  let id = 0
  const active = new Map()
  return {
    active,
    setTimeout(fn) { const handle = ++id; active.set(handle, fn); return handle },
    clearTimeout(id) { active.delete(id) },
    fire() { const pending = [...active.values()]; active.clear(); for (const fn of pending) fn() },
  }
}
function storage() {
  const data = new Map([['owner-untouched', 'not-a-room-token']])
  return { data, getItem: key => data.get(key) || null, setItem: (key, value) => data.set(key, value), removeItem: key => data.delete(key) }
}
class Events {
  constructor() { this.listeners = new Map() }
  addEventListener(name, fn) { if (!this.listeners.has(name)) this.listeners.set(name, new Set()); this.listeners.get(name).add(fn) }
  removeEventListener(name, fn) { this.listeners.get(name)?.delete(fn) }
  fire(name, event = {}) { for (const fn of [...(this.listeners.get(name) || [])]) fn({ type: name, ...event }) }
  listenerCount() { return [...this.listeners.values()].reduce((sum, value) => sum + value.size, 0) }
}
function sockets() {
  const all = []
  class Socket {
    static OPEN = 1
    constructor(url) { this.url = url; this.readyState = 0; this.bufferedAmount = 0; this.sent = []; this.closes = []; all.push(this) }
    send(data) { this.sent.push(JSON.parse(data)) }
    close(code, reason) { this.closes.push({ code, reason }); this.readyState = 3 }
    open() { this.readyState = 1; this.onopen?.({}) }
    message(data) { this.onmessage?.({ data: data instanceof ArrayBuffer ? data : typeof data === 'string' ? data : JSON.stringify(data) }) }
    end(code = 1006) { this.readyState = 3; this.onclose?.({ code }) }
  }
  return { all, Socket }
}
function sessionHarness(overrides = {}) {
  const clock = timers(), ws = sockets(), store = storage(), requests = [], states = [], media = [], bytes = [], cursors = []
  let response = { status: 200, json: async () => ({ token: 'ephemeral-room-token', member: { id: 'me', username: 'Viewer' } }) }
  const load = loader({ ...clock, WebSocket: ws.Socket, fetch: async (url, options) => { requests.push({ url, ...options }); return response }, ...overrides })
  const protocol = load('src/aggregate/protocol.ts')
  const urls = protocol.aggregateURLs({ origin: 'https://manager.test', pathname: '/prefix/room/demo/' })
  const token = new protocol.RoomToken(urls.base, store)
  const { RoomSession } = load('src/aggregate/session.ts')
  const hooks = { change: state => states.push(state), media: config => media.push(config), binary: value => bytes.push(value), cursor: value => cursors.push(value) }
  const session = new RoomSession(urls, token, hooks)
  async function connect() {
    await session.login('Viewer', 'room-password')
    const socket = ws.all[ws.all.length - 1]
    socket.open(); socket.message(hello)
    return socket
  }
  return { clock, ws, store, requests, states, media, bytes, cursors, load, protocol, urls, token, hooks, session, connect, respond: value => { response = value } }
}

test('canonical room paths derive credential-free HTTP/WSS endpoints, ignoring page query/hash', () => {
  const { aggregateURLs } = loader()('src/aggregate/protocol.ts')
  for (const origin of ['http://lan.test:8080', 'https://public.test']) {
    for (const pathname of ['/room/demo/', '/prefix/room/a%20b///']) {
      const urls = aggregateURLs({ origin, pathname, search: '?token=must-not-leak', hash: '#secret' })
      const base = origin + pathname.replace(/\/+$/, '') + '/__aggregate'
      assert.deepEqual(plain(urls), { base, login: base + '/login', logout: base + '/logout', stream: base.replace(/^http/, 'ws') + '/stream' })
      assert.equal(new URL(urls.stream).search, '')
      assert.equal(new URL(urls.stream).username, '')
    }
  }
  for (const pathname of ['/manage/', '/aggregate.html', '/room/', '/room/demo/nested/']) assert.throws(() => aggregateURLs({ origin: 'http://lan.test', pathname }), /canonical/)
})

test('room token isolation and denied storage retain only the in-memory ephemeral token', () => {
  const { RoomToken } = loader()('src/aggregate/protocol.ts'), store = storage()
  const first = new RoomToken('https://room.test/room/one/__aggregate', store)
  const other = new RoomToken('https://room.test/room/two/__aggregate', store)
  first.set('one'); other.set('two'); first.set(null)
  assert.equal(other.get(), 'two')
  assert.equal(store.data.get(other.key), 'two')
  assert.equal(store.data.get('owner-untouched'), 'not-a-room-token')
  const denied = new RoomToken('base', { getItem() { throw Error() }, setItem() { throw Error() }, removeItem() { throw Error() } })
  denied.set('RAM'); assert.equal(denied.get(), 'RAM'); denied.set(null); assert.equal(denied.get(), null)
})

test('form login sends only room credentials; same-origin WS sends token in its first auth frame', async () => {
  const h = sessionHarness(), socket = await h.connect()
  assert.equal(h.requests.length, 1)
  const request = h.requests[0]
  assert.equal(request.url, h.urls.login)
  assert.equal(request.method, 'POST')
  assert.equal(request.credentials, 'omit')
  assert.equal(request.redirect, 'error')
  assert.deepEqual(JSON.parse(request.body), { username: 'Viewer', password: 'room-password' })
  assert.equal(request.headers.Authorization, undefined)
  assert.equal(socket.url, 'wss://manager.test/prefix/room/demo/__aggregate/stream')
  assert.equal(socket.binaryType, 'arraybuffer')
  assert.deepEqual(socket.sent, [{ type: 'auth', token: 'ephemeral-room-token' }])
  assert.equal(h.session.state.phase, 'connected')
  const media = new ArrayBuffer(4)
  socket.message(media)
  assert.equal(h.bytes[0], media) // Identity proves routing only, not usable media.
  assert.deepEqual(plain(h.media), [hello])
  assert.equal([...h.store.data.values()].includes('room-password'), false)
  h.session.disconnect()
})

test('non-200, malformed JSON and network login failures remain ordinary visible form errors', async () => {
  for (const response of [
    { status: 401, json: async () => ({ message: 'Wrong room password' }) },
    { status: 403, json: async () => { throw Error('HTML proxy page') } },
    { status: 200, json: async () => '<html>Owner login</html>' },
    { status: 200, json: async () => ({}) },
  ]) {
    const h = sessionHarness(); h.respond(response)
    await h.session.login('Viewer', 'incorrect')
    assert.equal(h.session.state.phase, 'login')
    assert.equal(h.session.state.error, true)
    assert.ok(h.session.state.message)
    assert.equal(h.ws.all.length, 0)
    assert.equal(h.token.get(), null)
  }
  const h = sessionHarness({ fetch: async () => { throw Error('Network Error') } })
  await h.session.login('Viewer', 'password')
  assert.match(h.session.state.message, /Could not reach/)
  assert.equal(h.clock.active.size, 0)
})

test('only authoritative ownership permits input; loss clears keys/buttons and never requests ownership', async () => {
  const h = sessionHarness(), socket = await h.connect()
  h.session.control('request')
  assert.equal(h.session.controlled, false)
  assert.equal(h.session.pointer(pointer()), false)
  assert.equal(h.session.key(key()), false)
  assert.equal(h.session.clipboard('ignored'), false)
  socket.message({ type: 'state', controller_id: 'me', members: [{ id: 'me', username: 'Viewer' }] })
  assert.equal(h.session.controlled, true)
  assert.equal(h.session.key(key()), true)
  assert.equal(h.session.pointer(pointer('down', { x: 9000, y: -1 })), true)
  assert.deepEqual(socket.sent[socket.sent.length - 1], { type: 'pointer', ...pointer('down', { x: 1919, y: 0 }) })
  socket.message({ type: 'state', members: [] })
  assert.equal(h.session.controlled, true) // Partial updates do not imply ownership loss.
  socket.message({ type: 'state', controller_id: 'other' })
  assert.equal(h.session.controlled, false)
  assert.equal(h.session.dragging, false)
  assert.deepEqual(socket.sent.slice(-2), [
    { type: 'key', ...key('up') }, { type: 'pointer', ...pointer('up', { x: 1919, y: 0 }) },
  ])
  const length = socket.sent.length
  h.session.key(key('up')); h.session.pointer(pointer('up')); h.session.releaseInputs()
  assert.equal(socket.sent.length, length)
  assert.equal(socket.sent.filter(value => value.type === 'control' && value.action === 'request').length, 1)
  h.session.disconnect()
})

test('release control flushes held modifiers/buttons first; clipboard is host-only and bounded', async () => {
  const h = sessionHarness(), socket = await h.connect()
  socket.message({ type: 'state', controller_id: 'me' })
  h.session.key(key()); h.session.pointer(pointer()); h.session.clipboard('exact\nclipboard')
  assert.deepEqual(socket.sent[socket.sent.length - 1], { type: 'clipboard', text: 'exact\nclipboard' })
  assert.throws(() => h.session.clipboard('x'.repeat(65537)), /64 KiB/)
  h.session.control('release')
  assert.equal(h.session.controlled, false)
  assert.deepEqual(socket.sent.slice(-3), [{ type: 'key', ...key('up') }, { type: 'pointer', ...pointer('up') }, { type: 'control', action: 'release' }])
  h.session.disconnect()
})

test('disconnect cleans current state; reconnect validates token; auth failure removes only this room token', async () => {
  const h = sessionHarness(), first = await h.connect()
  first.message({ type: 'state', controller_id: 'me' }); h.session.key(key())
  first.end()
  assert.equal(h.session.state.phase, 'disconnected')
  assert.equal(h.session.state.memberId, null)
  assert.equal(h.session.state.desktop, null)
  assert.equal(h.session.controlled, false)
  assert.equal(h.token.get(), 'ephemeral-room-token')
  h.session.reconnect()
  const second = h.ws.all[1]; second.open()
  assert.deepEqual(second.sent, [{ type: 'auth', token: 'ephemeral-room-token' }])
  second.message(hello)
  assert.equal(h.session.state.memberId, 'me')
  second.message({ type: 'error', code: 'auth_failed', message: 'Please sign in again' })
  assert.equal(h.session.state.phase, 'login')
  assert.equal(h.session.state.message, 'Please sign in again')
  assert.equal(h.token.get(), null)
  assert.deepEqual([...h.store.data.keys()], ['owner-untouched'])
  assert.equal(h.clock.active.size, 0)
})

test('logout uses only Bearer room token then closes/clears even if server logout fails', async () => {
  for (const status of [200, 500]) {
    const h = sessionHarness(), socket = await h.connect()
    h.respond({ status, json: async () => true })
    await h.session.logout()
    const request = h.requests[1]
    assert.equal(request.url, h.urls.logout)
    assert.deepEqual(plain(request.headers), { Authorization: 'Bearer ephemeral-room-token' })
    assert.equal(request.credentials, 'omit')
    assert.equal(request.body, undefined)
    assert.equal(h.token.get(), null)
    assert.equal(h.session.state.phase, 'login')
    assert.equal(h.session.state.error, status !== 200)
    assert.equal(socket.closes.length, 1)
    assert.equal(h.store.data.has('owner-untouched'), true)
  }
})

test('a delayed logout response cannot erase a newer room login', async () => {
  let finishLogout, loginCount = 0
  const logoutGate = new Promise(resolve => { finishLogout = resolve })
  const h = sessionHarness({ fetch: async url => {
    if (url.endsWith('/logout')) return logoutGate
    return { status: 200, json: async () => ({ token: 'room-token-' + (++loginCount), member: { id: 'me', username: 'Viewer' } }) }
  } })
  await h.connect()
  const logout = h.session.logout()
  h.session.disconnect('Network interrupted logout', true)
  await h.session.login('Viewer', 'new-password')
  const current = h.ws.all[1]; current.open(); current.message(hello)
  finishLogout({ status: 200 }); await logout
  assert.equal(h.token.get(), 'room-token-2')
  assert.equal(h.session.state.phase, 'connected')
  h.session.disconnect()
})

test('stale login responses, old sockets, protocol errors, timeouts and input congestion cannot revive sessions', async () => {
  let resolveLogin
  const pending = new Promise(resolve => { resolveLogin = resolve })
  const stale = sessionHarness({ fetch: () => pending })
  const login = stale.session.login('Viewer', 'password')
  stale.session.disconnect()
  resolveLogin({ status: 200, json: async () => ({ token: 'stale' }) }); await login
  assert.equal(stale.token.get(), null)
  assert.equal(stale.ws.all.length, 0)
  for (const data of ['bad-json', new ArrayBuffer(1), { type: 'hello', mime: 'video/webm', width: 0, height: 1 }]) {
    const h = sessionHarness()
    await h.session.login('Viewer', 'password'); h.ws.all[0].open(); h.ws.all[0].message(data)
    assert.equal(h.session.state.phase, 'disconnected')
    assert.equal(h.session.state.error, true)
    assert.equal(h.clock.active.size, 0)
  }
  const h = sessionHarness(), socket = await h.connect()
  socket.message({ type: 'state', controller_id: 'me' })
  socket.bufferedAmount = 300000
  assert.equal(h.session.key(key()), false)
  assert.match(h.session.state.message, /congested/)
  assert.equal(h.session.controlled, false)
  h.session.reconnect(); h.clock.fire()
  assert.match(h.session.state.message, /did not start/)
  assert.equal(h.clock.active.size, 0)
})

test('media reset replaces the epoch before new bytes; invalid/repeated epochs fail explicitly', async () => {
  const h = sessionHarness(), socket = await h.connect()
  socket.message({ type: 'state', controller_id: 'me' }); h.session.key(key())
  socket.message({ ...hello, type: 'media-reset', epoch: 1, width: 1280, height: 720 })
  assert.equal(h.media.length, 2)
  assert.equal(h.session.state.desktop.width, 1280)
  assert.deepEqual(socket.sent[socket.sent.length - 1], { type: 'key', ...key('up') })
  socket.message(new ArrayBuffer(2)); assert.equal(h.bytes.length, 1)
  socket.message({ ...hello, type: 'media-reset', epoch: 1 })
  assert.equal(h.session.state.phase, 'disconnected')
  assert.match(h.session.state.message, /epoch/)
})

test('cursor messages preserve normalized image/hotspot data and observer position', async () => {
  const h = sessionHarness(), socket = await h.connect()
  const image = { type: 'cursor', action: 'image', png: 'iVBORw0KGgo=', width: 16, height: 16, hotspotX: 2, hotspotY: 3 }
  socket.message(image); socket.message({ type: 'cursor', action: 'position', x: 100, y: 200 })
  assert.deepEqual(plain(h.cursors), [image, { type: 'cursor', action: 'position', x: 100, y: 200 }])
  socket.message({ ...image, png: 'x'.repeat(400000) })
  assert.equal(h.session.state.phase, 'disconnected')
})

test('letterbox coordinate mapping handles offsets, scaling, edges and captured drags', () => {
  const { desktopPoint } = loader()('src/aggregate/protocol.ts')
  const box = { left: 10, top: 20, width: 1000, height: 1000 }, size = { width: 1920, height: 1080 }
  assert.deepEqual(plain(desktopPoint(510, 520, box, size)), { x: 960, y: 540 })
  assert.equal(desktopPoint(510, 100, box, size), null)
  assert.equal(desktopPoint(1010, 520, box, size), null)
  assert.deepEqual(plain(desktopPoint(5000, -100, box, size, true)), { x: 1919, y: 0 })
  assert.equal(desktopPoint(0, 0, { ...box, width: 0 }, size), null)
  assert.equal(desktopPoint(NaN, 0, box, size), null)
})

function mediaHarness(supported = true) {
  const clock = timers(), sources = [], created = [], revoked = [], failures = [], playRequired = []
  class Buffer extends Events {
    constructor() { super(); this.updating = false; this.ranges = []; this.appends = []; this.removes = []; this.aborts = 0 }
    get buffered() { return { length: this.ranges.length, start: i => this.ranges[i][0], end: i => this.ranges[i][1] } }
    appendBuffer(bytes) { assert.equal(this.updating, false); this.appends.push(bytes); this.updating = true }
    remove(start, end) { assert.equal(this.updating, false); this.removes.push([start, end]); this.updating = true }
    abort() { this.aborts++; this.updating = false; this.fire('abort') }
    finish(ranges = this.ranges) { this.ranges = ranges; this.updating = false; this.fire('updateend') }
  }
  class Source extends Events {
    static isTypeSupported(mime) { Source.checked = mime; return supported }
    constructor() { super(); this.readyState = 'closed'; this.removed = []; sources.push(this) }
    addSourceBuffer(mime) { this.mime = mime; this.buffer = new Buffer(); return this.buffer }
    removeSourceBuffer(buffer) { this.removed.push(buffer) }
    open() { this.readyState = 'open'; this.fire('sourceopen') }
  }
  class LocalURL extends URL {
    static createObjectURL(value) { const url = 'blob:test-' + created.length; created.push([url, value]); return url }
    static revokeObjectURL(value) { revoked.push(value) }
  }
  const video = Object.assign(new Events(), { readyState: 1, currentTime: 0, src: '', pauses: 0, loads: 0,
    play: async () => undefined, pause() { this.pauses++ }, removeAttribute() { this.src = '' }, load() { this.loads++ },
  })
  const { DesktopMedia, MEDIA_LIMITS } = loader({ ...clock, MediaSource: Source, URL: LocalURL })('src/aggregate/media.ts')
  const player = new DesktopMedia(video, message => failures.push(message), () => playRequired.push(true))
  return { clock, sources, created, revoked, failures, playRequired, video, player, limits: MEDIA_LIMITS, Source }
}

test('MSE accepts the server MIME, serializes append/remove and keeps a rolling live buffer', () => {
  const h = mediaHarness(), mime = 'video/webm; codecs="vp9,opus"'
  h.player.start(mime)
  const first = new ArrayBuffer(2), next = new ArrayBuffer(3)
  h.player.push(first); h.player.push(next)
  assert.equal(h.sources[0].buffer, undefined)
  h.sources[0].open()
  const buffer = h.sources[0].buffer
  assert.equal(h.Source.checked, mime)
  assert.equal(buffer.appends.length, 1)
  assert.equal(buffer.appends[0], first)
  buffer.finish([[0, 30]])
  assert.equal(h.video.currentTime, 29.75)
  assert.deepEqual(buffer.removes, [[0, 20]])
  assert.equal(buffer.appends.length, 1)
  buffer.finish([[20, 30]])
  assert.equal(buffer.appends[1], next)
  buffer.finish([[20, 30.1]])
  assert.equal(h.clock.active.size, 0)
  h.player.stop()
  assert.deepEqual(h.revoked, ['blob:test-0'])
  assert.equal(h.video.src, '')
  assert.equal(buffer.listenerCount(), 0)
  assert.equal(h.sources[0].listenerCount(), 0)
  assert.equal(h.video.listenerCount(), 0)
})

test('MSE queue bytes/chunk limits and source/append stalls fail closed instead of dropping WebM deltas', () => {
  for (const kind of ['bytes', 'chunks', 'source-stall', 'append-stall']) {
    const h = mediaHarness(); h.player.start(hello.mime)
    if (kind === 'bytes') h.player.push(new ArrayBuffer(h.limits.bytes + 1))
    if (kind === 'chunks') for (let i = 0; i <= h.limits.chunks; i++) h.player.push(new ArrayBuffer(1))
    if (kind === 'source-stall') h.clock.fire()
    if (kind === 'append-stall') { h.sources[0].open(); h.player.push(new ArrayBuffer(1)); h.clock.fire() }
    assert.equal(h.failures.length, 1, kind)
    assert.equal(h.clock.active.size, 0)
    assert.equal(h.video.src, '')
    assert.equal(h.player.queue.length, 0)
    assert.equal(h.player.activeBytes, 0)
    assert.deepEqual(h.revoked, ['blob:test-0'])
  }
})

test('MSE unsupported type, decode failure, append failure, epoch cleanup and autoplay rejection are explicit', async () => {
  const unsupported = mediaHarness(false); unsupported.player.start('video/unknown')
  assert.match(unsupported.failures[0], /cannot play/)
  assert.equal(unsupported.sources.length, 0)
  const h = mediaHarness(); h.player.start(hello.mime); h.sources[0].open()
  const old = h.sources[0].buffer
  h.player.push(new ArrayBuffer(1)); h.player.start(hello.mime)
  assert.equal(old.aborts, 1)
  assert.equal(old.listenerCount(), 0)
  assert.equal(h.player.queue.length, 0)
  assert.deepEqual(h.revoked, ['blob:test-0'])
  h.sources[1].open(); h.video.play = async () => { throw Error('autoplay denied') }
  h.video.fire('canplay'); await new Promise(setImmediate)
  assert.equal(h.playRequired.length, 1)
  h.sources[1].buffer.fire('error')
  assert.match(h.failures[0], /decoded/)
  assert.equal(h.clock.active.size, 0)
  const broken = mediaHarness(); broken.player.start(hello.mime); broken.sources[0].open()
  broken.sources[0].buffer.appendBuffer = () => { throw Error('QuotaExceededError') }
  broken.player.push(new ArrayBuffer(1))
  assert.match(broken.failures[0], /failed or filled/)
})

function surfaceHarness(h) {
  const doc = Object.assign(new Events(), { activeElement: null, hidden: false })
  const win = new Events(), urls = [], revoked = []
  class LocalURL extends URL {
    static createObjectURL() { const url = 'blob:cursor-' + urls.length; urls.push(url); return url }
    static revokeObjectURL(url) { revoked.push(url) }
  }
  const classes = new Set(), surface = Object.assign(new Events(), {
    style: {}, clientHeight: 1000,
    classList: { toggle(name, value) { if (value) classes.add(name); else classes.delete(name) } },
    focus() { doc.activeElement = surface },
    getBoundingClientRect: () => ({ left: 0, top: 0, width: 1000, height: 1000 }),
    setPointerCapture(id) { this.capture = id }, hasPointerCapture(id) { return this.capture === id }, releasePointerCapture() { this.capture = null },
  })
  const image = { style: {}, hidden: true, removeAttribute() { this.src = '' } }
  const { DesktopSurface } = loader({ ...h.clock, window: win, document: doc, URL: LocalURL })('src/aggregate/desktop.ts')
  const ui = new DesktopSurface(surface, image, h.session, () => undefined)
  function fire(type, extra = {}) {
    let prevented = false
    surface.fire(type, { clientX: 500, clientY: 500, button: 0, pointerId: 1, preventDefault() { prevented = true }, stopPropagation() { return undefined }, ...extra })
    return prevented
  }
  return { doc, win, surface, image, ui, fire, classes, urls, revoked }
}

test('desktop keyboard/pointer/wheel capture is focused and controller-only; moves flush before keys', async () => {
  const h = sessionHarness(), socket = await h.connect(), ui = surfaceHarness(h)
  assert.equal(ui.fire('pointerdown'), false)
  socket.message({ type: 'state', controller_id: 'me' })
  assert.equal(ui.fire('keydown', key()), false)
  assert.equal(ui.fire('pointerdown', { clientY: 10 }), false) // top letterbox
  assert.equal(ui.fire('pointerdown'), true)
  assert.equal(ui.doc.activeElement, ui.surface)
  ui.fire('pointermove', { clientX: 600 })
  ui.fire('keydown', key())
  assert.deepEqual(socket.sent.slice(-2).map(value => value.type), ['pointer', 'key'])
  assert.equal(socket.sent[socket.sent.length - 2].x, 1152)
  assert.equal(ui.fire('wheel', { deltaMode: 1, deltaX: 2, deltaY: -3 }), true)
  assert.equal(socket.sent[socket.sent.length - 1].deltaY, -48)
  ui.surface.fire('blur')
  assert.equal(socket.sent[socket.sent.length - 2].action, 'up')
  assert.equal(socket.sent[socket.sent.length - 1].action, 'up')
  ui.fire('pointermove', { clientX: 700 })
  socket.message({ type: 'state', controller_id: 'other' }); h.clock.fire()
  assert.equal(socket.sent.filter(value => value.type === 'control').length, 0)
  ui.ui.dispose(); h.session.disconnect()
  assert.equal(ui.surface.listenerCount(), 0)
  assert.equal(ui.doc.listenerCount(), 0)
  assert.equal(ui.win.listenerCount(), 0)
})

test('cursor overlay uses letterbox scale/hotspot, follows host locally, and revokes URLs on replacement/cleanup', async () => {
  const h = sessionHarness(), socket = await h.connect(), ui = surfaceHarness(h)
  const image = { type: 'cursor', action: 'image', width: 16, height: 16, hotspotX: 2, hotspotY: 3, png: 'iVBORw0KGgo=' }
  ui.ui.cursor(image); ui.image.onload()
  ui.ui.cursor({ type: 'cursor', action: 'position', x: 960, y: 540 })
  assert.equal(ui.image.hidden, false)
  assert.equal(parseFloat(ui.image.style.left), (960 - 2) * 1000 / 1920)
  assert.equal(parseFloat(ui.image.style.top), 218.75 + (540 - 3) * 1000 / 1920)
  socket.message({ type: 'state', controller_id: 'me' }); ui.ui.sync()
  assert.equal(ui.classes.has('hide-local-cursor'), true)
  ui.fire('pointerdown', { clientX: 600 })
  const left = ui.image.style.left
  ui.ui.cursor({ type: 'cursor', action: 'position', x: 1, y: 1 })
  assert.equal(ui.image.style.left, left) // stale echoed host position never overrides local position
  ui.ui.cursor(image); ui.image.onload()
  assert.deepEqual(ui.revoked, ['blob:cursor-0'])
  ui.ui.cursor({ ...image, width: 0, height: 0, png: '' })
  assert.equal(ui.image.hidden, true)
  assert.equal(ui.classes.has('hide-local-cursor'), true)
  ui.ui.dispose(); h.session.disconnect()
  assert.deepEqual(ui.revoked, ['blob:cursor-0', 'blob:cursor-1'])
  assert.equal(ui.classes.has('hide-local-cursor'), false)
})

test('standalone page has normal room form fields and fixed public namespace without Owner boot or framework', () => {
  const config = require('../vue.config.js')
  assert.equal(config.publicPath, './')
  assert.equal(config.pages.app.entry, 'src/main.ts')
  assert.equal(config.pages.app.filename, 'index.html')
  assert.equal(config.pages.aggregate.publicPath, '/__aggregate/')
  assert.equal(config.pages.aggregate.filename, 'aggregate.html')
  assert.deepEqual(config.pages.aggregate.chunks, ['aggregate-vendors', 'aggregate'])
  const html = readFileSync(resolve(root, 'src/aggregate/index.html'), 'utf8')
  assert.match(html, /name="username" autocomplete="username"/)
  assert.match(html, /name="password" type="password" autocomplete="current-password"/)
  for (const file of ['main.ts', 'session.ts', 'protocol.ts', 'media.ts', 'desktop.ts']) {
    const source = readFileSync(resolve(root, 'src/aggregate', file), 'utf8')
    assert.doesNotMatch(source, /from ['"](?:vue|axios|.*\/auth)['"]|RTCPeerConnection|console\.|localStorage/)
  }
})
