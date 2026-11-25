const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const { createRequire } = require('node:module')
const { resolve } = require('node:path')
const { test } = require('node:test')
const { runInNewContext } = require('node:vm')
const axios = require('axios')
const ts = require('typescript')
const Vue = require('vue')
const compiler = require('vue-template-compiler')

const root = resolve(__dirname, '..')
const config = ts.readConfigFile(resolve(root, 'tsconfig.json'), ts.sys.readFile).config
const options = ts.convertCompilerOptionsFromJson(config.compilerOptions, root).options

// Same installed TS/Vue compiler pattern as agent-access.test.cjs; no test framework/browser dependency.
function load(file, mocks = {}, globals = {}) {
  const filename = resolve(root, file)
  const source = readFileSync(filename, 'utf8')
  const output = ts.transpileModule(source, {
    compilerOptions: { ...options, module: ts.ModuleKind.CommonJS }, fileName: filename,
  }).outputText
  const exports = {}
  const localRequire = createRequire(filename)
  runInNewContext(output, {
    exports, Error, console, URL, URLSearchParams,
    require: name => Object.prototype.hasOwnProperty.call(mocks, name) ? mocks[name] : localRequire(name),
    ...globals,
  }, { filename })
  return exports
}

const plain = value => JSON.parse(JSON.stringify(value))
const settle = () => new Promise(setImmediate)
function deferred() {
  let resolve
  const promise = new Promise(yes => { resolve = yes })
  return { promise, resolve }
}

function harness(pathname = '/manage/', overrides = {}) {
  const prefix = pathname.replace(/\/index\.html$/, '').replace(/\/+$/, '')
  const redirects = [], requests = [], instances = [], failures = []
  const location = {
    origin: 'http://manager.test', pathname, search: '?view=rooms', hash: '#agent',
    href: 'http://manager.test' + pathname + '?view=rooms#agent',
    replace: url => redirects.push(url),
  }
  const state = {
    status: { mode: 'form', authenticated: true, username: 'owner', login_url: prefix + '/auth/login', logout_url: prefix + '/auth/logout' },
    statusCode: 200, apiCode: 401, statusError: null, gate: null,
    ...overrides,
  }
  const adapter = async request => {
    requests.push(request)
    const isStatus = request.url === location.origin + prefix + '/auth/status'
    if (isStatus && state.gate) await state.gate
    if (isStatus && state.statusError) throw state.statusError
    const status = isStatus ? state.statusCode : state.apiCode
    const response = { config: request, status, statusText: String(status), headers: {}, data: isStatus ? state.status : 'session expired' }
    if (status >= 400) {
      const error = new axios.AxiosError('Request failed with status code ' + status, 'ERR_BAD_REQUEST', request, undefined, response)
      failures.push(error)
      throw error
    }
    return response
  }
  // Real Axios interceptors and adapters, with fresh clients per test and no network requests.
  const client = axios.create({ adapter })
  client.isAxiosError = axios.isAxiosError
  client.create = options => {
    const instance = axios.create({ ...options, adapter })
    instances.push(instance)
    return instance
  }
  const auth = load('src/auth.ts', { axios: client }, {
    location,
    localStorage: new Proxy({}, { get: () => { throw new Error('Authentication must not access localStorage') } }),
  })
  const agent = () => load('src/api/agent.ts', { axios: client, '@/auth': auth })
  const generated = () => {
    const base = load('src/api/base.ts', { axios: client })
    const common = load('src/api/common.ts', { './base': base })
    const { Configuration } = load('src/api/configuration.ts')
    const { RoomsApi } = load('src/api/api.ts', { axios: client, './base': base, './common': common })
    return new RoomsApi(new Configuration({ basePath: auth.managerBasePath }))
  }
  return { prefix, location, state, client, auth, agent, generated, redirects, requests, instances, failures }
}

function boot(h) {
  const mounts = []
  const app = { attributes: {}, textContent: '', setAttribute(name, value) { this.attributes[name] = value } }
  class StubVue {
    static config = {}
    static use(plugin) { return plugin }
    constructor(options) { this.options = options }
    $mount(selector) {
      assert.equal(selector, '#app')
      mounts.push(this.options.render((component, options) => options.props))
    }
  }
  load('src/main.ts', {
    vue: StubVue, './App.vue': {}, './plugins/filters.ts': {}, './plugins/sweetalert': {},
    './store': {}, './plugins/vuetify': {}, './auth': h.auth, '@/assets/styles/main.scss': {},
  }, { document: { getElementById: id => { assert.equal(id, 'app'); return app } } })
  return { mounts, app }
}

test('public status uses the manager prefix including root/index.html, with no auth headers or stored credentials', async () => {
  for (const pathname of ['/manage/', '/manage', '/prefix/manage///', '/', '/manage/index.html', '/index.html']) {
    const h = harness(pathname)
    const headers = plain(h.client.defaults.headers)
    const status = await h.auth.initializeManagerAuth()
    assert.deepEqual(plain(status), h.state.status)
    assert.equal(h.auth.managerBasePath, h.location.origin + h.prefix)
    assert.equal(h.requests.length, 1)
    assert.equal(h.requests[0].url, h.location.origin + h.prefix + '/auth/status')
    assert.equal(h.requests[0].timeout, 15000)
    assert.equal(h.requests[0].headers.get('Authorization'), undefined)
    assert.equal(h.requests[0].auth, undefined)
    assert.equal(h.requests[0].withCredentials, undefined) // Same-origin cookies need no cross-origin setting.
    assert.deepEqual(plain(h.client.defaults.headers), headers)
    assert.deepEqual(h.redirects, [])
  }
})

test('return paths preserve local UI query/hash and reject auth, API, CDP, outside-prefix and external targets', () => {
  for (const pathname of ['/manage/', '/prefix/manage/', '/']) {
    const h = harness(pathname)
    const fallback = h.prefix + '/'
    const index = h.prefix + '/index.html'
    for (const candidate of [fallback, fallback + '?tab=rooms#agent', index + '?q=%E4%B8%AD#agent']) {
      assert.equal(h.auth.safeReturnPath(candidate), candidate)
    }
    for (const candidate of [
      '', 'https://evil.test/', 'http://manager.test' + fallback, '//evil.test/', '/\\evil.test/',
      '/outside/', h.prefix + '-other/', h.prefix + '/auth/login', h.prefix + '/auth/logout',
      h.prefix + '/api/rooms', h.prefix + '/api/agent/rooms/stable/keys', h.prefix + '/cdp/json/version',
      '/rooms/stable/cdp/json/version', '/stable/devtools/browser/id', h.prefix + '/%61uth/login',
      fallback + '?x=\r\nLocation:evil', fallback + '?x=\u0000',
    ]) {
      assert.equal(h.auth.safeReturnPath(candidate), fallback, candidate)
    }
  }
})

test('Vue/API boot waits for status; authenticated mode receives the username and server logout URL', async () => {
  const gate = deferred()
  const h = harness('/manage/', { gate: gate.promise })
  const ui = boot(h)
  await settle()
  assert.equal(ui.mounts.length, 0)
  assert.deepEqual(h.requests.map(request => request.url), ['http://manager.test/manage/auth/status'])
  gate.resolve()
  await settle()
  assert.equal(ui.mounts.length, 1)
  assert.deepEqual(plain(ui.mounts[0].auth), h.state.status)
  assert.equal(ui.app.textContent, '')
})

test('unauthenticated form mode navigates before mounting Vue or booting manager APIs, including the deprecated root', async () => {
  for (const pathname of ['/manage/', '/prefix/manage/', '/', '/manage/index.html']) {
    const h = harness(pathname)
    h.state.status.authenticated = false
    const ui = boot(h)
    await settle()
    assert.equal(ui.mounts.length, 0)
    assert.equal(h.requests.length, 1)
    assert.deepEqual(h.redirects, [h.prefix + '/auth/login?next=' + encodeURIComponent(pathname + '?view=rooms#agent')])
  }
})

test('basic, proxy and none keep their existing boot/401 flows even if status is unauthenticated', async () => {
  for (const mode of ['basic', 'proxy', 'none']) {
    const h = harness()
    h.state.status.mode = mode
    h.state.status.authenticated = false
    const ui = boot(h)
    await settle()
    assert.equal(ui.mounts.length, 1)
    await assert.rejects(h.generated().roomsList(), error => error.response.status === 401)
    await assert.rejects(h.agent().listAgentKeys('stable'), error => error.response.status === 401)
    assert.deepEqual(h.redirects, [])
  }
})

test('real generated default Axios and handwritten Agent instance share one redirect for concurrent expired-session 401s', async () => {
  for (const pathname of ['/manage/', '/prefix/manage/', '/']) {
    const h = harness(pathname)
    await h.auth.initializeManagerAuth()
    const generated = h.generated(), agent = h.agent()
    assert.equal(h.instances.length, 1)
    assert.equal(h.instances[0].interceptors.response.handlers.length, 1)
    const results = await Promise.allSettled([
      generated.roomsList(), generated.roomClone('stable /名', { name: 'separate-room' }), agent.listAgentKeys('stable /名'),
      agent.createAgentKey('stable', 3600, ''), agent.revokeAgentKey('stable', 'key /id'),
    ])
    for (const result of results) {
      assert.equal(result.status, 'rejected')
      assert.equal(result.reason.response.status, 401)
      assert.equal(result.reason.response.data, 'session expired')
      assert.ok(h.failures.includes(result.reason)) // Preserve the actual Axios error, do not parse/login via the API.
    }
    assert.deepEqual(h.redirects, [h.prefix + '/auth/login?next=' + encodeURIComponent(pathname + '?view=rooms#agent')])
    assert.ok(h.requests.some(request => request.url === h.auth.managerBasePath + '/api/rooms'))
    assert.ok(h.requests.some(request => request.url.includes('/api/agent/rooms/stable%20%2F%E5%90%8D/keys')))
    const clone = h.requests.find(request => request.url.endsWith('/api/rooms/stable%20%2F%E5%90%8D/clone'))
    assert.equal(clone.method, 'post')
    assert.deepEqual(JSON.parse(clone.data), { name: 'separate-room' })
    for (const request of h.requests) {
      assert.equal(request.headers.get('Authorization'), undefined)
      assert.equal(request.headers.get('X-CSRF-Token'), undefined)
      assert.equal(request.auth, undefined)
    }
    assert.equal(h.client.defaults.headers.common.Authorization, undefined)
  }
})

test('the generated default client also redirects when it is the only failing API client', async () => {
  const h = harness()
  await h.auth.initializeManagerAuth()
  await assert.rejects(h.generated().roomsList(), error => error.response.status === 401)
  assert.equal(h.redirects.length, 1)
  await assert.rejects(h.agent().listAgentKeys('stable'), error => error.response.status === 401)
  assert.equal(h.redirects.length, 1)
})

test('only form-mode same-origin manager API 401s redirect; public CDP Bearer and other failures remain untouched', async () => {
  for (const pathname of ['/manage/', '/']) {
    const h = harness(pathname)
    await assert.rejects(h.client.get(h.auth.managerBasePath + '/api/rooms')) // No initialized form status yet.
    await h.auth.initializeManagerAuth()
    for (const url of [
      'http://other.test' + h.prefix + '/api/rooms', '//other.test' + h.prefix + '/api/rooms',
      h.prefix + '/api-other', h.prefix + '/auth/unknown', '/rooms/stable/cdp/json/version',
      '/stable/json/version', '/stable/devtools/browser/id',
    ]) {
      await assert.rejects(h.client.get(url, { headers: { Authorization: 'Bearer ephemeral-cdp-only' } }), error => error.response.status === 401)
    }
    for (const status of [403, 404, 500]) {
      h.state.apiCode = status
      await assert.rejects(h.agent().listAgentKeys('stable'), error => error.response.status === status)
    }
    assert.deepEqual(h.redirects, [])
  }
})

test('HTML, malformed status and unsafe server auth URLs fail explicitly instead of assuming an auth mode', async () => {
  const valid = harness().state.status
  for (const status of [
    '<!doctype html><form>login</form>', null, {}, { ...valid, mode: 'sso' },
    { ...valid, authenticated: 'true' }, { ...valid, username: {} },
    { ...valid, login_url: '//other.test/auth/login' }, { ...valid, logout_url: '/auth/logout' },
  ]) {
    const h = harness('/manage/', { status })
    await assert.rejects(h.auth.initializeManagerAuth(), /Invalid Manager authentication status response/)
    assert.deepEqual(h.redirects, [])
    assert.equal(h.requests.length, 1)
  }
})

test('status network/HTTP/HTML failures are visible, do not mount Vue, and do not start an API error storm', async () => {
  for (const overrides of [
    { statusError: new Error('Network Error') }, { statusCode: 503 }, { statusCode: 401 },
    { status: '<!doctype html><form>login</form>' },
  ]) {
    const h = harness('/manage/', overrides)
    const ui = boot(h)
    await settle()
    assert.equal(ui.mounts.length, 0)
    assert.equal(ui.app.attributes.role, 'alert')
    assert.match(ui.app.textContent, /Could not read Manager authentication status.*Refresh the page to retry/)
    assert.equal(h.requests.length, 1)
    assert.deepEqual(h.redirects, [])
  }
})

function descendants(node) {
  if (!node) return []
  return [node, ...(node.children || []).flatMap(descendants)]
}

test('rendered logout is a native POST form only in form mode, using the server URL and escaped username text', () => {
  const template = compiler.parseComponent(readFileSync(resolve(root, 'src/App.vue'), 'utf8')).template.content
  const compiled = compiler.compile(template)
  assert.deepEqual(compiled.errors, [])
  const render = compiler.compileToFunctions(template).render
  for (const prefix of ['/manage', '/nested/manage', '']) {
    for (const mode of ['form', 'basic', 'proxy', 'none']) {
      const auth = { mode, authenticated: true, username: '<owner&name>', logout_url: prefix + '/auth/logout' }
      const instance = new Vue({ data: { auth }, render })
      const nodes = descendants(instance._render())
      const forms = nodes.filter(node => node.tag === 'form')
      assert.equal(forms.length, mode === 'form' ? 1 : 0)
      assert.ok(nodes.some(node => node.text === '<owner&name>'))
      if (mode === 'form') {
        assert.equal(forms[0].data.attrs.method, 'post')
        assert.equal(forms[0].data.attrs.action, auth.logout_url)
        assert.equal(forms[0].data.on, undefined) // No submit interception/JS auth protocol.
        const button = descendants(forms[0]).find(node => node.tag === 'v-btn')
        assert.equal(button.data.attrs.type, 'submit')
        assert.ok(descendants(button).some(node => node.text === 'Sign out'))
      } else {
        assert.ok(!nodes.some(node => node.text === 'Sign out'))
      }
      instance.$destroy()
    }
  }
})

test('development proxy forwards public auth endpoints as well as the manager API', () => {
  const source = readFileSync(resolve(root, 'vue.config.js'), 'utf8')
  const module = { exports: {} }
  runInNewContext(source, {
    module, require: () => ({ defineConfig: config => config }), process: { env: { API_PROXY: 'http://backend.test/manage' } },
  })
  const proxy = module.exports.devServer.proxy
  const [pattern] = Object.keys(proxy)
  for (const path of ['/auth/status', '/auth/login', '/auth/logout', '/api/rooms', '/api/events?sse']) {
    assert.match(path, new RegExp(pattern))
  }
  assert.doesNotMatch('/authorization', new RegExp(pattern))
  assert.equal(proxy[pattern].target, 'http://backend.test/manage')
})
