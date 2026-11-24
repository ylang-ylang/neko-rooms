const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const { createRequire } = require('node:module')
const { resolve } = require('node:path')
const { test } = require('node:test')
const { runInNewContext } = require('node:vm')
const ts = require('typescript')
const compiler = require('vue-template-compiler')

const root = resolve(__dirname, '..')
const config = ts.readConfigFile(resolve(root, 'tsconfig.json'), ts.sys.readFile).config
const options = ts.convertCompilerOptionsFromJson(config.compilerOptions, root).options

// Exercise the actual TypeScript and Vue class methods using existing compiler dependencies.
// No browser/test-framework dependency is needed; real clipboard paste is a browser acceptance check.
function load(file, mocks = {}, globals = {}) {
  const filename = resolve(root, file)
  const source = readFileSync(filename, 'utf8')
  const code = file.endsWith('.vue') ? compiler.parseComponent(source).script.content : source
  const output = ts.transpileModule(code, {
    compilerOptions: { ...options, module: ts.ModuleKind.CommonJS },
    fileName: filename,
  }).outputText
  const exports = {}
  const localRequire = createRequire(filename)
  runInNewContext(output, {
    exports, Error, console,
    require: name => Object.prototype.hasOwnProperty.call(mocks, name) ? mocks[name] : localRequire(name),
    ...globals,
  }, { filename })
  return exports
}

const plain = value => JSON.parse(JSON.stringify(value))
// VM/host promises cross realms; drain their microtasks before checking non-blocking copy feedback.
const settle = () => new Promise(setImmediate)
function deferred() {
  let resolve, reject
  const promise = new Promise((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}
function textarea(value = 'canonical\nprompt') {
  const calls = []
  return {
    value, calls,
    focus: () => calls.push('focus'),
    select: () => calls.push('select'),
    setSelectionRange: (start, end) => calls.push([start, end]),
  }
}

const issued = {
  key_id: 'issued-id', key: 'ephemeral-test-value', room_name: 'stable-room',
  created_at: '2026-01-01T00:00:00Z', expires_at: '2026-01-01T01:00:00Z',
  cdp_url: 'returned-by-backend', prompt: 'Backend canonical text\n原样交接，不生成前端模板。',
}
const metadata = {
  key_id: issued.key_id, room_name: issued.room_name, label: 'test label',
  created_at: issued.created_at, expires_at: issued.expires_at, active_connections: 2,
}
function dialog(overrides = {}, copy = async () => true) {
  const service = {
    createAgentKey: async () => ({ ...issued }),
    listAgentKeys: async () => [],
    revokeAgentKey: async () => undefined,
    agentErrorMessage: error => error.message,
    ...overrides,
  }
  const Component = load('src/components/AgentAccess.vue', {
    '@/api/agent': service, '@/utils/clipboard': { copyPrompt: copy },
  }).default
  const instance = new Component({ propsData: { roomName: 'stable-room' } })
  instance.$refs.prompt = textarea()
  instance.$nextTick = async () => { instance.$refs.prompt.value = instance.issued.prompt }
  return instance
}

test('API uses management pathname, encoded stable names, exact payloads and no global auth', async () => {
  for (const pathname of ['/manage/', '/prefix/manage///', '/']) {
    const calls = []
    const defaults = { headers: { sentinel: 'unchanged' } }
    const mock = {
      defaults,
      create: options => {
        assert.deepEqual(plain(options), { timeout: 15000 })
        return Object.fromEntries(['get', 'post', 'delete'].map(method => [method, async (...args) => {
          calls.push([method, ...args])
          return { data: method === 'post' ? issued : { keys: [metadata] } }
        }]))
      },
    }
    let interceptorInstalls = 0
    const api = load('src/api/agent.ts', { axios: mock, '@/auth': {
      managerBasePath: 'http://manager.test' + pathname.replace(/\/+$/, ''),
      installManagerAuthInterceptor: () => { interceptorInstalls++ },
    } })
    assert.equal(interceptorInstalls, 1)
    assert.equal(await api.createAgentKey('stable /名', 3600, '  label  '), issued)
    assert.equal(await api.createAgentKey('stable /名', 900, '  '), issued)
    assert.deepEqual(await api.listAgentKeys('stable /名'), [metadata])
    await api.revokeAgentKey('stable /名', 'key /id')
    const url = 'http://manager.test' + pathname.replace(/\/+$/, '') + '/api/agent/rooms/stable%20%2F%E5%90%8D/keys'
    assert.deepEqual(plain(calls), [
      ['post', url, { ttl_seconds: 3600, label: 'label' }],
      ['post', url, { ttl_seconds: 900 }],
      ['get', url], ['delete', url + '/key%20%2Fid'],
    ])
    assert.deepEqual(defaults, { headers: { sentinel: 'unchanged' } })
  }
})

test('API errors retain HTTP details or the network error message', () => {
  const api = load('src/api/agent.ts', { axios: {
    create: () => ({}), isAxiosError: error => Boolean(error.isAxiosError),
  }, '@/auth': { installManagerAuthInterceptor: () => undefined } })
  for (const data of ['room unavailable\n', { error: 'room unavailable' }, { message: 'room unavailable' }]) {
    assert.equal(api.agentErrorMessage({ isAxiosError: true, response: { status: 404, data } }),
      'HTTP 404：room unavailable')
  }
  assert.equal(api.agentErrorMessage({ isAxiosError: true, response: { status: 500, data: {} } }), 'HTTP 500')
  assert.equal(api.agentErrorMessage(new Error('Network Error')), 'Network Error')
})

test('HTTP clipboard fallback selects exact text and copies synchronously in the click gesture', async () => {
  let gesture = true
  const area = textarea()
  const helper = load('src/utils/clipboard.ts', {}, {
    document: { execCommand: command => { assert.equal(command, 'copy'); assert.equal(gesture, true); return true } },
    navigator: {}, // Ordinary HTTP LAN origins need not expose navigator.clipboard.
  })
  const copied = helper.copyPrompt(area)
  gesture = false
  assert.deepEqual(area.calls, ['focus', 'select', [0, area.value.length]])
  assert.equal(await copied, true)
})

test('modern clipboard is available when selection copy is unsupported', async () => {
  const area = textarea()
  const copied = []
  const helper = load('src/utils/clipboard.ts', {}, {
    document: { execCommand: () => { throw new Error('unsupported') } },
    navigator: { clipboard: { writeText: async text => copied.push(text) } },
  })
  assert.equal(await helper.copyPrompt(area), true)
  assert.deepEqual(copied, [area.value])
})

test('denied or absent clipboard reports failure and leaves text selected for manual copy', async () => {
  for (const navigator of [{}, { clipboard: { writeText: async () => { throw new Error('denied') } } }]) {
    const area = textarea()
    const helper = load('src/utils/clipboard.ts', {}, { document: { execCommand: () => false }, navigator })
    assert.equal(await helper.copyPrompt(area), false)
    assert.deepEqual(area.calls, ['focus', 'select', [0, area.value.length]])
  }
})

test('dialog defaults to 1h, offers valid TTL presets, and uses the returned prompt verbatim', async () => {
  const calls = []
  const copies = []
  const instance = dialog({ createAgentKey: async (...args) => { calls.push(args); return { ...issued } } },
    async area => { copies.push(area.value); return true })
  assert.equal(instance.ttlSeconds, 3600)
  assert.deepEqual(plain(instance.ttlPresets.map(preset => preset.value)), [900, 3600, 28800])
  instance.label = 'optional'
  await instance.CreateKey()
  assert.deepEqual(calls, [['stable-room', 3600, 'optional']])
  assert.equal(instance.issued.prompt, issued.prompt)
  assert.deepEqual(copies, [issued.prompt])
  await settle()
  assert.equal(instance.copyStatus, 'copied')
  assert.equal(instance.busy, false)
  instance.ttlSeconds = 28800
  await instance.CreateKey()
  assert.deepEqual(calls[1], ['stable-room', 28800, 'optional'])
  instance.$destroy()
})

test('unresolved clipboard permission cannot hold the creation/close actions busy', async () => {
  const permission = deferred()
  const instance = dialog({}, () => permission.promise)
  await instance.CreateKey()
  assert.equal(instance.busy, false)
  assert.equal(instance.copyStatus, 'idle')
  let closed = false
  instance.$on('close', () => { closed = true })
  instance.onDialogInput(false)
  assert.equal(closed, true)
  permission.resolve(false)
  await settle()
  assert.equal(instance.copyStatus, 'manual')
  instance.$destroy()
})

test('creation and list failures remain visible, preserve the current prompt and release busy state', async () => {
  const instance = dialog({
    createAgentKey: async () => { throw new Error('create rejected') },
    listAgentKeys: async () => { throw new Error('list rejected') },
  })
  instance.issued = { ...issued }
  await instance.CreateKey()
  await instance.LoadKeys()
  assert.match(instance.createError, /create rejected/)
  assert.match(instance.listError, /list rejected/)
  assert.equal(instance.issued.prompt, issued.prompt)
  assert.equal(instance.busy, false)
  assert.equal(instance.loading, false)
  instance.$destroy()
})

test('a stale list response cannot resurrect keys after a newer refresh', async () => {
  const first = deferred(), second = deferred()
  let call = 0
  const instance = dialog({ listAgentKeys: () => (++call === 1 ? first : second).promise })
  const older = instance.LoadKeys(), newer = instance.LoadKeys()
  second.resolve([])
  await newer
  first.resolve([metadata])
  await older
  assert.deepEqual(plain(instance.keys), [])
  assert.equal(instance.loading, false)
  instance.$destroy()
})

test('revocation waits for DELETE, then removes metadata and clears a revoked issuance', async () => {
  const deletion = deferred()
  const calls = []
  const instance = dialog({ revokeAgentKey: (...args) => { calls.push(args); return deletion.promise } })
  instance.keys = [{ ...metadata }]
  instance.issued = { ...issued }
  const result = instance.RevokeKey(metadata.key_id)
  assert.equal(instance.keys.length, 1)
  assert.equal(instance.busy, true)
  deletion.resolve()
  await result
  assert.deepEqual(calls, [['stable-room', metadata.key_id]])
  assert.deepEqual(plain(instance.keys), [])
  assert.equal(instance.issued, null)
  assert.equal(instance.busy, false)
  instance.$destroy()
})

test('failed revocation retains the key and prompt and shows its error', async () => {
  const instance = dialog({ revokeAgentKey: async () => { throw new Error('revoke rejected') } })
  instance.keys = [{ ...metadata }]
  instance.issued = { ...issued }
  await instance.RevokeKey(metadata.key_id)
  assert.equal(instance.keys.length, 1)
  assert.equal(instance.issued.prompt, issued.prompt)
  assert.match(instance.revokeError, /revoke rejected/)
  assert.equal(instance.busy, false)
  instance.$destroy()
})

test('ported room defaults keep Profile/Downloads independent across instances and resets', () => {
  const enums = {
    BrowserPolicyTypeEnum: { chromium: 'chromium', firefox: 'firefox' },
    RoomMountTypeEnum: { private: 'private', public: 'public' },
  }
  const state = load('src/store/state.ts', { '@/api/index': enums }).state
  state.roomsConfig.neko_images = ['truenas-neko-chromium-cdp:3.2.0-1']
  const Component = load('src/components/RoomsCreate.vue', {
    '@/api/index': enums, '@/utils/random': { randomPassword: () => '' },
  }).default
  const create = () => new Component({ beforeCreate() { this.$store = { state } } })
  const first = create(), second = create()
  first.data.neko_image = state.roomsConfig.neko_images[0]
  assert.equal(first.data.api_version, 3)
  assert.equal(first.data.screen, '1920x1080@30')
  assert.equal(first.data.resources.shm_size, 2147483648)
  assert.equal(first.browserPolicyContent.developer_tools, true)
  assert.equal(first.browserPolicyContent.persistent_data, true)
  assert.equal(first.browserPolicyConfig.profile, '/home/neko/profile')
  assert.notEqual(first.data.mounts, second.data.mounts)
  assert.notEqual(first.data.resources, second.data.resources)
  assert.notEqual(first.browserPolicyContent.extensions, second.browserPolicyContent.extensions)
  first.onPersistentDataUpdate(true)
  assert.equal(first.data.mounts.length, 2) // Enabling persistence does not duplicate Profile.
  first.onPersistentDataUpdate(false)
  assert.deepEqual(plain(first.data.mounts), [
    { type: 'private', host_path: '/downloads', container_path: '/home/neko/Downloads' },
  ])
  assert.equal(second.data.mounts.length, 2)
  first.$refs.form = { resetValidation: () => undefined }
  first.Clear()
  assert.equal(first.data.mounts.length, 2)
  assert.notEqual(first.data.mounts, state.defaultRoomSettings.mounts)
  first.$destroy()
  second.$destroy()
})

test('Vue templates compile; Agent action uses room.name and lists no raw keys', () => {
  for (const file of ['AgentAccess.vue', 'RoomsList.vue', 'RoomsCreate.vue']) {
    const source = readFileSync(resolve(root, 'src/components', file), 'utf8')
    const template = compiler.parseComponent(source).template.content
    assert.deepEqual(compiler.compile(template).errors, [], file)
    if (file === 'RoomsList.vue') {
      assert.match(template, /agentRoomName = item\.name/)
      assert.match(template, /AgentAccess v-if="agentRoomName"/)
    } else if (file === 'AgentAccess.vue') {
      assert.match(template, /readonly rows="8"/)
      assert.doesNotMatch(template, /\{\{\s*key\.(key|prompt|cdp_url)\s*\}\}/)
      assert.match(template, /Manager 重启会使所有临时 Key 失效/)
      assert.match(template, /自动复制未成功/)
    }
  }
})
