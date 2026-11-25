const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const { createRequire } = require('node:module')
const { resolve } = require('node:path')
const { spawnSync } = require('node:child_process')
const { test } = require('node:test')
const { runInNewContext } = require('node:vm')
const ts = require('typescript')
const axios = require('axios')
const yaml = require('js-yaml')

const root = resolve(__dirname, '..')
const config = ts.readConfigFile(resolve(root, 'tsconfig.json'), ts.sys.readFile).config
const options = ts.convertCompilerOptionsFromJson(config.compilerOptions, root).options
const plain = value => JSON.parse(JSON.stringify(value))
function load(file, mocks = {}) {
  const filename = resolve(root, file)
  const exports = {}
  const localRequire = createRequire(filename)
  runInNewContext(ts.transpileModule(readFileSync(filename, 'utf8'), {
    compilerOptions: { ...options, module: ts.ModuleKind.CommonJS }, fileName: filename,
  }).outputText, {
    exports, URL, URLSearchParams,
    require: name => Object.prototype.hasOwnProperty.call(mocks, name) ? mocks[name] : localRequire(name),
  }, { filename })
  return exports
}
function generated(client = axios) {
  const base = load('src/api/base.ts', { axios: client })
  const common = load('src/api/common.ts', { './base': base })
  return {
    ...load('src/api/configuration.ts'),
    ...load('src/api/api.ts', { axios: client, './base': base, './common': common }),
  }
}

test('clone SDK uses only encoded source ID and exact name JSON on existing same-origin Axios', async () => {
  const calls = []
  const client = axios.create({ adapter: async request => {
    calls.push(request)
    return { data: { id: 'created-clone' }, status: 201, statusText: 'Created', headers: {}, config: request }
  } })
  const { RoomsApi, RoomsApiFactory, RoomsApiFp, Configuration } = generated(client)
  for (const prefix of ['/manage', '/nested/manage', '']) {
    const config = new Configuration({ basePath: 'https://manager.test' + prefix })
    for (const api of [new RoomsApi(config), RoomsApiFactory(config), { roomClone: async (...args) => (await RoomsApiFp(config).roomClone(...args))() }]) {
      const result = await api.roomClone('source /名', { name: 'exact-new-name' })
      assert.deepEqual(plain(result.data), { id: 'created-clone' })
      const request = calls[calls.length - 1]
      assert.equal(request.method, 'post')
      assert.equal(request.url, 'https://manager.test' + prefix + '/api/rooms/source%20%2F%E5%90%8D/clone')
      assert.deepEqual(JSON.parse(request.data), { name: 'exact-new-name' })
      assert.equal(request.headers.get('Content-Type'), 'application/json')
      assert.equal(request.headers.get('Authorization'), undefined)
      assert.equal(request.headers.get('Origin'), undefined) // Browser supplies its real same-origin Origin.
      assert.equal(request.auth, undefined)
      assert.equal(request.withCredentials, undefined) // Native same-origin cookie behavior, not another credential path.
      assert.equal(request.params, undefined)
    }
  }
  assert.equal(calls.length, 9)
  const api = new RoomsApi(new Configuration({ basePath: 'https://manager.test/manage' }))
  await assert.rejects(api.roomClone(undefined, { name: 'new-name' }), /roomId/)
  await assert.rejects(api.roomClone('source', undefined), /roomCloneRequest/)
  assert.equal(calls.length, 9)
})

test('Vuex clone delegates one create-and-start operation, without settings, keys, start calls or partial entry mutation', async () => {
  const calls = []
  class RoomsApi {
    async roomClone(...args) { calls.push(args); return { data: { id: 'created-clone' } } }
  }
  class Configuration { constructor(options) { Object.assign(this, options) } }
  class Store { constructor(options) { Object.assign(this, options) } }
  const { default: store } = load('src/store/index.ts', {
    vue: { use: () => undefined }, vuex: { Store },
    '@/api/index': { Configuration, RoomsApi, ConfigApi: class {}, DefaultApi: class {} },
    './state': { state: {} }, '@/auth': { managerBasePath: 'https://manager.test/manage' },
  })
  const commits = []
  const result = await store.actions.ROOMS_CLONE({ commit: (...args) => commits.push(args) }, {
    roomId: 'source-id', name: 'new-room',
    // Even unexpected callers cannot add source configuration or temporary Agent state to this request.
    ignored: 'not-sent',
  })
  assert.deepEqual(plain(calls), [['source-id', { name: 'new-room' }]])
  assert.deepEqual(plain(result), { id: 'created-clone' })
  assert.deepEqual(commits, [])
})

test('clone model and bounded generation match stable OpenAPI without replacing SDK/auth/Agent files', () => {
  const document = yaml.load(readFileSync(resolve(root, '../OpenApi.yml'), 'utf8'))
  const operation = document.paths['/api/rooms/{roomId}/clone'].post
  assert.equal(operation.operationId, 'roomClone')
  assert.equal(operation.requestBody.required, true)
  assert.equal(operation.responses['201'].content['application/json'].schema.$ref, '#/components/schemas/RoomEntry')
  assert.ok(operation.responses['409'])
  const file = ts.createSourceFile('api.ts', readFileSync(resolve(root, 'src/api/api.ts'), 'utf8'), ts.ScriptTarget.Latest, true)
  const model = file.statements.find(node => ts.isInterfaceDeclaration(node) && node.name.text === 'RoomCloneRequest')
  assert.equal(model.members.length, 1)
  assert.equal(model.members[0].name.text, 'name')
  assert.equal(model.members[0].questionToken, undefined)
  assert.equal(model.members[0].type.kind, ts.SyntaxKind.StringKeyword)
  const result = spawnSync(process.execPath, [resolve(root, 'scripts/generate-room-clone.cjs'), '--check'], { encoding: 'utf8' })
  assert.equal(result.status, 0, result.stderr || result.stdout)
})
