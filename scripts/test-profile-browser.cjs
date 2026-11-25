#!/usr/bin/env node
// Opt-in real-browser feature test. Uses only newly owned canary deployments,
// profiles and an ephemeral HTTP origin; it never imports cookies into a target.
const assert = require('node:assert/strict')
const { execFileSync, spawn } = require('node:child_process')
const { randomBytes } = require('node:crypto')
const http = require('node:http')
const path = require('node:path')
const WebSocket = require('../client/node_modules/ws')

const base = process.env.NEKO_PROFILE_TEST_URL
const storage = process.env.NEKO_PROFILE_TEST_STORAGE
const originHost = process.env.NEKO_PROFILE_TEST_ORIGIN_HOST
const image = process.env.NEKO_PROFILE_TEST_IMAGE
assert.ok(base && storage && originHost && image, 'Set NEKO_PROFILE_TEST_URL, STORAGE, ORIGIN_HOST and IMAGE for an isolated local Docker manager')
const prefix = 'profile-canary-' + randomBytes(5).toString('hex')
const account = randomBytes(24).toString('hex')
const ownedRooms = new Map()
const ownedProfiles = new Set()
const sockets = new Set()
const headers = { 'Content-Type': 'application/json', Origin: new URL(base).origin }

async function api(method, endpoint, body, expected = 200) {
  const response = await fetch(base + '/api' + endpoint, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(90000) })
  assert.equal(response.status, expected, `${method} ${endpoint}: HTTP ${response.status}`)
  return expected === 204 || expected >= 400 ? undefined : response.json()
}
class CDP {
  constructor(socket) {
    this.socket = socket
    this.id = 0
    this.pending = new Map()
    this.events = new Map()
    socket.on('message', data => {
      const message = JSON.parse(String(data))
      if (message.id) {
        const pending = this.pending.get(message.id)
        if (!pending) return
        this.pending.delete(message.id)
        clearTimeout(pending.timer)
        if (message.error) pending.reject(new Error('CDP rejected ' + pending.method))
        else pending.resolve(message.result)
      } else {
        const key = (message.sessionId || '') + ':' + message.method
        const resolve = this.events.get(key)
        if (resolve) { this.events.delete(key); resolve(message.params) }
      }
    })
  }
  call(method, params = {}, sessionId) {
    const id = ++this.id
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => { this.pending.delete(id); reject(new Error('CDP timeout: ' + method)) }, 20000)
      this.pending.set(id, { resolve, reject, timer, method })
      this.socket.send(JSON.stringify({ id, method, params, sessionId }))
    })
  }
  event(method, sessionId) {
    return new Promise((resolve, reject) => {
      const key = (sessionId || '') + ':' + method
      const timer = setTimeout(() => { this.events.delete(key); reject(new Error('CDP event timeout: ' + method)) }, 20000)
      this.events.set(key, value => { clearTimeout(timer); resolve(value) })
    })
  }
  async evaluate(expression, sessionId) {
    const result = await this.call('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true }, sessionId)
    assert.ok(!result.exceptionDetails && result.result.value === true, 'Real browser identity assertion failed')
  }
}
async function browser(room) {
  const key = await api('POST', '/agent/rooms/' + room.name + '/keys', { ttl_seconds: 900 }, 201)
  const response = await fetch(key.cdp_url + 'json/version', { headers: { Authorization: 'Bearer ' + key.key } })
  assert.equal(response.status, 200, 'Owned browser discovery unavailable')
  const version = await response.json()
  const socket = new WebSocket(version.webSocketDebuggerUrl, { headers: { Authorization: 'Bearer ' + key.key } })
  await new Promise((resolve, reject) => { socket.once('open', resolve); socket.once('error', reject) })
  sockets.add(socket)
  const cdp = new CDP(socket)
  const targets = await cdp.call('Target.getTargets')
  const page = targets.targetInfos.find(target => target.type === 'page' && /^https?:/.test(target.url)) || targets.targetInfos.find(target => target.type === 'page')
  assert.ok(page, 'Owned browser has no page')
  const attached = await cdp.call('Target.attachToTarget', { targetId: page.targetId, flatten: true })
  return { cdp, session: attached.sessionId }
}
// Subscribe before first Start; Docker's health event avoids startup polling.
function healthy(id) {
  const events = spawn('docker', ['events', '--filter', 'type=container', '--filter', 'container=' + id, '--filter', 'event=health_status', '--format', '{{.Action}}'])
  return new Promise((resolve, reject) => {
    let done = false
    const timer = setTimeout(() => { done = true; events.kill(); reject(new Error('Owned browser health event timed out')) }, 90000)
    events.once('exit', code => { if (!done) { clearTimeout(timer); reject(new Error('Owned browser health event subscription exited: ' + code)) } })
    events.once('error', error => { clearTimeout(timer); reject(error) })
    events.stdout.on('data', data => {
      if (String(data).includes('health_status: healthy')) { done = true; clearTimeout(timer); events.kill(); resolve() }
    })
  })
}
async function navigate(client, url) {
  await client.cdp.call('Page.enable', {}, client.session)
  const loaded = client.cdp.event('Page.loadEventFired', client.session)
  await client.cdp.call('Page.navigate', { url }, client.session)
  await loaded
}
const settings = name => ({
  api_version: 3, name, neko_image: image, max_connections: 2,
  user_pass: randomBytes(16).toString('hex'), admin_pass: randomBytes(16).toString('hex'),
  screen: '1280x720@30', video_codec: 'VP8', audio_codec: 'OPUS',
  resources: { shm_size: 536870912 },
  mounts: [{ type: 'private', host_path: '/profile', container_path: '/home/neko/profile' }, { type: 'private', host_path: '/downloads', container_path: '/home/neko/Downloads' }],
})
function remember(room) { ownedRooms.set(room.id, room); ownedProfiles.add(room.profile_id); return room }
async function remove(room) { await api('DELETE', '/rooms/' + room.id, undefined, 204); ownedRooms.delete(room.id) }
function fixture(profileId, action) {
  assert.ok(ownedProfiles.has(profileId), 'Refusing storage access outside owned profiles')
  const code = `import json,pathlib,sqlite3,sys\nr=pathlib.Path('/owned')\np=r/'profile'\n` + (action === 'seed' ?
    `p.mkdir(exist_ok=True)\n(p/'Local State').write_text(json.dumps({'neko_profile_fixture':True}))\ne=p/'Default/Extensions/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/1.0'\ne.mkdir(parents=True,exist_ok=True)\n(e/'manifest.json').write_text('{"manifest_version":3,"name":"Owned profile fixture","version":"1.0"}')\nd=r/'downloads';d.mkdir(exist_ok=True);(d/'owned-fixture.txt').write_text('owned download')\n` :
    `assert json.loads((p/'Local State').read_text()).get('neko_profile_fixture') is True\nassert (p/'Default/Extensions/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/1.0/manifest.json').exists()\nassert (r/'downloads/owned-fixture.txt').read_text()=='owned download'\nconn=sqlite3.connect('file:'+str(p/'Default/History')+'?mode=ro',uri=True)\nassert any('/fixture' in row[0] for row in conn.execute('SELECT url FROM urls'))\n`) + `print('Owned opaque profile artifacts: OK')`
  // This helper never launches Chromium and mounts only this test's new profile.
  const output = execFileSync('docker', ['run', '--rm', '--network', 'none', '-v', path.join(storage, 'profiles', profileId) + ':/owned', '--entrypoint', 'python3', image, '-c', code], { encoding: 'utf8' })
  assert.match(output, /artifacts: OK/)
  if (action === 'seed') execFileSync('docker', ['run', '--rm', '--network', 'none', '-v', path.join(storage, 'profiles', profileId) + ':/owned', '--entrypoint', '/bin/chown', image, '-R', '1000:1000', '/owned/profile', '/owned/downloads'])
}
const readIdentity = `(async()=>{const who=await(await fetch('/whoami')).json();const value=await new Promise((resolve,reject)=>{const open=indexedDB.open('profile-fixture',1);open.onerror=()=>reject(open.error);open.onsuccess=()=>{const db=open.result;const tx=db.transaction('identity');const q=tx.objectStore('identity').get('account');q.onsuccess=()=>{db.close();resolve(q.result)};q.onerror=()=>reject(q.error)}});return who.authenticated&&localStorage.getItem('identity')==='original'&&value==='original'})()`

async function main() {
  let unexpectedNetwork = 0
  const origin = http.createServer((request, response) => {
    if (request.url.startsWith('/.neko-clone/')) unexpectedNetwork++
    if (request.url === '/login') {
      response.setHeader('Set-Cookie', [`profile-persistent=${account}; HttpOnly; Path=/; SameSite=Lax; Max-Age=3600`, `profile-session=${account}; HttpOnly; Path=/; SameSite=Lax`])
      response.end('logged in')
    } else if (request.url === '/whoami') {
      const jar = new Map((request.headers.cookie || '').split(';').map(item => item.trim().split('=')))
      response.setHeader('Content-Type', 'application/json')
      response.end(JSON.stringify({ authenticated: jar.get('profile-persistent') === account && jar.get('profile-session') === account }))
    } else {
      response.setHeader('Content-Type', 'text/html')
      response.end('<!doctype html><title>Owned profile fixture</title><button id="login" onclick="window.loggedIn=fetch(\'/login\',{method:\'POST\'}).then(r=>r.ok)">Log in</button>')
    }
  })
  await new Promise(resolve => origin.listen(0, '0.0.0.0', resolve))
  const url = `http://${originHost}:${origin.address().port}/fixture`
  let success = false
  try {
    const sourceSettings = settings(prefix + '-source')
    const source = remember(await api('POST', '/rooms?start=false', sourceSettings))
    fixture(source.profile_id, 'seed')
    const ready = healthy(source.id)
    await api('POST', '/rooms/' + source.id + '/start', undefined, 204)
    await ready
    const sourceBrowser = await browser(source)
    await navigate(sourceBrowser, url)
    await sourceBrowser.cdp.evaluate(`(async()=>{document.getElementById('login').click();if(!await window.loggedIn)return false;localStorage.setItem('identity','original');await new Promise((resolve,reject)=>{const open=indexedDB.open('profile-fixture',1);open.onupgradeneeded=()=>open.result.createObjectStore('identity');open.onerror=()=>reject(open.error);open.onsuccess=()=>{const db=open.result;const tx=db.transaction('identity','readwrite');tx.objectStore('identity').put('original','account');tx.oncomplete=()=>{db.close();resolve()};tx.onerror=()=>reject(tx.error)}});return true})()`, sourceBrowser.session)
    await sourceBrowser.cdp.evaluate(readIdentity, sourceBrowser.session)
    const conflicting = { ...sourceSettings, name: prefix + '-conflict', profile_id: source.profile_id }
    await api('POST', '/rooms', conflicting, 409)
    await api('DELETE', '/profiles/' + source.profile_id, undefined, 409)
    console.log('Real origin login + localStorage/IndexedDB; conflicting use and bound deletion denied: OK')

    const clone = remember(await api('POST', '/rooms/' + source.id + '/clone', { name: prefix + '-clone' }, 201))
    assert.notEqual(clone.profile_id, source.profile_id)
    const cloneBrowser = await browser(clone)
    await navigate(cloneBrowser, url)
    await cloneBrowser.cdp.evaluate(readIdentity, cloneBrowser.session)
    await cloneBrowser.cdp.evaluate(`localStorage.setItem('identity','clone-only');true`, cloneBrowser.session)
    await sourceBrowser.cdp.evaluate(`localStorage.getItem('identity')==='original'`, sourceBrowser.session)
    console.log('Clone forks independent durable identity without target cookie import: OK')
    await remove(source)
    const profiles = await api('GET', '/profiles')
    assert.ok(profiles.some(profile => profile.id === source.profile_id && !profile.room_id))
    fixture(source.profile_id, 'verify')
    const replacement = remember(await api('POST', '/rooms', { ...sourceSettings, name: prefix + '-replacement', profile_id: source.profile_id }))
    assert.equal(replacement.profile_id, source.profile_id)
    const replacementBrowser = await browser(replacement)
    await navigate(replacementBrowser, url)
    await replacementBrowser.cdp.evaluate(readIdentity, replacementBrowser.session)
    assert.equal(unexpectedNetwork, 0, 'Storage restoration contacted the helper origin')
    fixture(replacement.profile_id, 'verify')
    console.log('Deployment removed and same profile reused: server Whoami authenticated; LS/IDB, Local State, extension, history and Downloads retained: OK')
    success = true
  } finally {
    for (const socket of sockets) socket.close()
    if (success) {
      for (const room of ownedRooms.values()) await remove(room)
      for (const id of ownedProfiles) await api('DELETE', '/profiles/' + id, undefined, 204)
      console.log('Owned canary deployments and profiles explicitly deleted: OK')
    }
    origin.closeAllConnections()
    await new Promise(resolve => origin.close(resolve))
  }
}
main().catch(error => { console.error(error.message); process.exitCode = 1 })
