const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const { createRequire } = require('node:module')
const { resolve } = require('node:path')
const { test } = require('node:test')
const { runInNewContext } = require('node:vm')
const ts = require('typescript')
const compiler = require('vue-template-compiler')
const Vue = require('vue')
const { rooms, pinnedImage } = require('./fixtures/rooms-list.cjs')

const root = resolve(__dirname, '..')
const filename = resolve(root, 'src/components/RoomsList.vue')
const source = compiler.parseComponent(readFileSync(filename, 'utf8'))
const config = ts.readConfigFile(resolve(root, 'tsconfig.json'), ts.sys.readFile).config
const options = ts.convertCompilerOptionsFromJson(config.compilerOptions, root).options
const compiled = compiler.compileToFunctions(source.template.content)
const exportsObject = {}
const localRequire = createRequire(filename)
const action = Vue.extend({ props: ['action', 'roomId', 'disabled'] })
const mocks = {
  '@/components/RoomActionBtn.vue': action,
  '@/components/RoomInfo.vue': Vue.extend({ props: ['roomId'] }),
  '@/components/AgentAccess.vue': Vue.extend({ props: ['roomName'] }),
}
runInNewContext(ts.transpileModule(source.script.content, {
  compilerOptions: { ...options, module: ts.ModuleKind.CommonJS }, fileName: filename,
}).outputText, {
  exports: exportsObject,
  require: name => mocks[name] || localRequire(name),
}, { filename })
const RoomsList = exportsObject.default
Vue.filter('timeago', value => 'relative ' + value)
const plain = value => JSON.parse(JSON.stringify(value))
const elements = nodes => nodes.filter(node => node.tag)
const text = node => node.text || (node.children || []).map(text).join('')
function list(usesMux = true) {
  return new RoomsList({
    ...compiled,
    beforeCreate() { this.$store = { state: { rooms, roomsConfig: { uses_mux: usesMux } } } },
  })
}
function table(instance) { return elements(instance._render().children)[0] }
function slot(instance, name, item = rooms[0]) {
  return elements(table(instance).data.scopedSlots['item.' + name]({ item }))
}

test('room columns keep original entries and optional max connections', () => {
  for (const usesMux of [true, false]) {
    const instance = list(usesMux)
    assert.equal(instance.rooms, rooms)
    assert.equal(table(instance).data.attrs.items, rooms)
    assert.deepEqual(plain(instance.headers.map(header => header.value)), [
      'url', 'name', ...(!usesMux ? ['max_connections'] : []),
      'neko_image', 'status', 'created', 'actions', 'agent', 'destroy',
    ])
    instance.$destroy()
  }
})

test('long names and pinned images retain full visible text and full image title', () => {
  const instance = list()
  for (const room of rooms) {
    assert.equal(text(slot(instance, 'name', room)[0]), room.name)
    const image = slot(instance, 'neko_image', room)[0]
    const [recreate, label] = elements(image.children)
    assert.deepEqual(plain(recreate.componentOptions.propsData), { action: 'recreate', roomId: room.id })
    assert.equal(text(label), pinnedImage)
    assert.equal(label.data.attrs.title, pinnedImage)
    assert.equal(text(slot(instance, 'status', room)[0]).trim(), room.status)
    assert.equal(table(instance).data.scopedSlots['item.created']({ item: room })[0].text.trim(), 'relative ' + room.created)
  }
  instance.$destroy()
})

test('all four actions preserve room IDs and running, stopped and paused availability', () => {
  const instance = list()
  for (const [item, disabled] of [
    [rooms[0], [true, false, false, false]],
    [rooms[1], [false, true, true, true]],
    [{ ...rooms[1], paused: true }, [false, false, true, true]],
  ]) {
    const controls = elements(slot(instance, 'actions', item)[0].children)
    assert.deepEqual(plain(controls.map(node => node.componentOptions.propsData)),
      ['start', 'stop', 'pause', 'restart'].map((action, index) => ({ action, roomId: item.id, disabled: disabled[index] })))
    assert.equal(slot(instance, 'destroy', item)[0].componentOptions.propsData.action, 'remove')
    assert.equal(slot(instance, 'destroy', item)[0].componentOptions.propsData.roomId, item.id)
  }
  instance.$destroy()
})

test('deployment and Agent still open the selected stable room without changing its data', () => {
  const instance = list()
  for (const item of rooms) {
    const deployment = slot(instance, 'url', item)
    const info = deployment[0].data.scopedSlots.activator({ on: {}, attrs: {} })[0]
    info.data.on.click()
    assert.equal(instance.roomId, item.id)
    assert.equal(instance.dialog, true)
    const link = deployment[1].data.scopedSlots.activator({ on: {}, attrs: {} })[0]
    assert.equal(link.data.attrs.href, item.url)
    assert.equal(link.data.attrs.disabled, !item.running)
    const agent = slot(instance, 'agent', item)[0]
    assert.equal(agent.data.attrs.disabled, false)
    agent.data.on.click()
    assert.equal(instance.agentRoomName, item.name)
    const dialog = elements(instance._render().children).find(node => node.componentOptions?.propsData?.roomName)
    assert.equal(dialog.componentOptions.propsData.roomName, item.name)
    dialog.componentOptions.listeners.close()
    assert.equal(instance.agentRoomName, '')
    assert.equal(item.neko_image, pinnedImage)
  }
  assert.equal(slot(instance, 'agent', { ...rooms[0], name: '' })[0].data.attrs.disabled, true)
  instance.$destroy()
})

test('layout rules are scoped, wrap identifiers and leave action buttons on one line', () => {
  assert.deepEqual(compiler.compile(source.template.content).errors, [])
  assert.equal(source.styles.length, 1)
  assert.equal(source.styles[0].scoped, true)
  const style = source.styles[0].content
  assert.match(style, /\.room-name\s*\{[^}]*min-width:[^}]*overflow-wrap: anywhere/s)
  assert.match(style, /\.room-image\s*\{[^}]*max-width:/s)
  assert.match(style, /\.room-image span\s*\{[^}]*min-width: 0;[^}]*overflow-wrap: anywhere/s)
  assert.match(style, /\.room-actions\s*\{[^}]*white-space: nowrap/s)
  assert.doesNotMatch(style, /overflow:\s*hidden|table-layout:\s*fixed/)
})
