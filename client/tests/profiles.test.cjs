const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const { createRequire } = require('node:module')
const { resolve } = require('node:path')
const { spawnSync } = require('node:child_process')
const { test } = require('node:test')
const { runInNewContext } = require('node:vm')
const ts = require('typescript')
const compiler = require('vue-template-compiler')
const axios = require('axios')
const root = resolve(__dirname, '..')
const config = ts.readConfigFile(resolve(root, 'tsconfig.json'), ts.sys.readFile).config
const options = ts.convertCompilerOptionsFromJson(config.compilerOptions, root).options
function load(file, mocks = {}) {
  const filename = resolve(root, file)
  const source = readFileSync(filename, 'utf8')
  const script = file.endsWith('.vue') ? compiler.parseComponent(source).script.content : source
  const exports = {}
  const localRequire = createRequire(filename)
  runInNewContext(ts.transpileModule(script, { compilerOptions: { ...options, module: ts.ModuleKind.CommonJS }, fileName: filename }).outputText,
    { exports, URL, URLSearchParams, require: name => mocks[name] || localRequire(name) }, { filename })
  return exports
}

test('bounded profile SDK uses existing same-origin transport for list/create/delete', async () => {
  const calls = []
  const client = axios.create({ adapter: async request => {
    calls.push(request)
    return { data: [], status: 200, statusText: 'OK', headers: {}, config: request }
  } })
  const base = load('src/api/base.ts', { axios: client })
  const common = load('src/api/common.ts', { './base': base })
  const { RoomsApi, RoomsApiFactory, RoomsApiFp } = load('src/api/api.ts', { axios: client, './base': base, './common': common })
  const { Configuration } = load('src/api/configuration.ts')
  const config = new Configuration({ basePath: 'https://manager.test/nested/manage' })
  for (const api of [new RoomsApi(config), RoomsApiFactory(config), {
    profilesList: async () => (await RoomsApiFp(config).profilesList())(),
    profileCreate: async (...args) => (await RoomsApiFp(config).profileCreate(...args))(),
    profileDelete: async (...args) => (await RoomsApiFp(config).profileDelete(...args))(),
  }]) {
    await api.profilesList()
    await api.profileCreate({ name: 'Retained account' })
    await api.profileDelete('stable /id')
  }
  assert.equal(calls.length, 9)
  for (let index = 0; index < calls.length; index += 3) {
    assert.equal(calls[index].url, 'https://manager.test/nested/manage/api/profiles')
    assert.equal(calls[index].method, 'get')
    assert.equal(calls[index + 1].method, 'post')
    assert.deepEqual(JSON.parse(calls[index + 1].data), { name: 'Retained account' })
    assert.equal(calls[index + 2].url, 'https://manager.test/nested/manage/api/profiles/stable%20%2Fid')
    assert.equal(calls[index + 2].method, 'delete')
    for (const request of calls.slice(index, index + 3)) assert.equal(request.headers.get('Authorization'), undefined)
  }
  const result = spawnSync(process.execPath, [resolve(root, 'scripts/generate-profiles.cjs'), '--check'], { encoding: 'utf8' })
  assert.equal(result.status, 0, result.stderr || result.stdout)
})

test('Profiles management requires explicit confirmation and blocks bound deletion', async () => {
  const filename = 'src/components/Profiles.vue'
  const component = load(filename).default
  assert.deepEqual(compiler.compile(compiler.parseComponent(readFileSync(resolve(root, filename), 'utf8')).template.content).errors, [])
  const calls = [], confirmations = []
  let confirmed = false
  const instance = new component({ beforeCreate() {
    this.$store = { state: { roomsConfig: { storage_enabled: true }, profiles: [] }, dispatch: async (...args) => { calls.push(args) } }
    this.$swal = async options => { confirmations.push(options); return { isConfirmed: confirmed } }
  } })
  const available = { id: 'owned-profile', name: 'Account' }
  await instance.Delete({ ...available, room_id: 'bound' })
  assert.equal(confirmations.length, 0)
  await instance.Delete(available)
  assert.equal(calls.length, 0)
  assert.match(confirmations[0].text, /owned-profile.*cookies.*history.*extensions.*Downloads.*cannot be undone/)
  confirmed = true
  await instance.Delete(available)
  assert.deepEqual(JSON.parse(JSON.stringify(calls)), [['PROFILES_DELETE', 'owned-profile'], ['PROFILES_LOAD']])
  instance.name = '  New account  '
  await instance.Create()
  assert.deepEqual(JSON.parse(JSON.stringify(calls.slice(2))), [['PROFILES_CREATE', 'New account'], ['PROFILES_LOAD']])
  assert.equal(instance.name, '')
  instance.$destroy()
})
