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
const nameRules = {}
runInNewContext(ts.transpileModule(readFileSync(resolve(root, 'src/utils/room-name.ts'), 'utf8'), {
  compilerOptions: { ...options, module: ts.ModuleKind.CommonJS },
}).outputText, { exports: nameRules })
const mocks = {
  '@/utils/room-name': nameRules,
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
function list(usesMux = true, dispatch = async () => ({ id: 'new-room' })) {
  return new RoomsList({
    ...compiled,
    beforeCreate() { this.$store = { state: { rooms, roomsConfig: { uses_mux: usesMux } }, dispatch } },
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

test('all five actions preserve room IDs and running, stopped and paused availability', () => {
  const instance = list()
  for (const [item, disabled] of [
    [rooms[0], [true, false, false, false]],
    [rooms[1], [false, true, true, true]],
    [{ ...rooms[1], paused: true }, [false, false, true, true]],
  ]) {
    const controls = elements(slot(instance, 'actions', item)[0].children)
    assert.equal(controls.length, 5)
    assert.deepEqual(plain(controls.slice(0, 4).map(node => node.componentOptions.propsData)),
      ['start', 'stop', 'pause', 'restart'].map((action, index) => ({ action, roomId: item.id, disabled: disabled[index] })))
    const clone = controls[4].data.scopedSlots.activator({ on: {}, attrs: {} })[0]
    assert.equal(clone.data.attrs['aria-label'], '复制房间 ' + item.name)
    clone.data.on.click()
    assert.equal(instance.cloneSource, item)
    assert.equal(instance.cloneName, item.name + '-copy')
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

function descendants(node) {
  return [node, ...(node.children || []).flatMap(descendants)]
}
function cloneDialog(instance) {
  return elements(instance._render().children).find(node => node.data?.attrs?.['aria-labelledby'] === 'room-clone-title')
}
function prepareClone(instance, name = 'new-room') {
  instance.OpenClone(rooms[0])
  instance.cloneName = name
  instance.$refs.cloneForm = { validate: () => instance.cloneNameRules.every(rule => rule(instance.cloneName) === true) }
}
function deferred() {
  let resolve
  const promise = new Promise(yes => { resolve = yes })
  return { promise, resolve }
}

test('clone dialog contains only the new name and native Enter submit, with autofocus and selection', () => {
  const instance = list()
  prepareClone(instance)
  const dialog = cloneDialog(instance)
  assert.equal(dialog.data.attrs.value, true)
  assert.equal(dialog.data.attrs.persistent, false)
  const nodes = descendants(dialog)
  const fields = nodes.filter(node => node.tag === 'v-text-field')
  assert.equal(fields.length, 1)
  assert.equal(fields[0].data.attrs.label, '新房间名称')
  assert.equal(fields[0].data.attrs.autofocus, '')
  assert.equal(fields[0].data.attrs.autocomplete, 'off')
  let selected = 0
  fields[0].data.on.focus({ target: { select: () => selected++ } })
  assert.equal(selected, 1)
  assert.equal(nodes.filter(node => node.tag === 'v-form').length, 1)
  assert.equal(typeof nodes.find(node => node.tag === 'v-form').data.on.submit, 'function')
  assert.equal(nodes.find(node => node.tag === 'v-btn' && text(node) === '复制并启动').data.attrs.type, 'submit')
  assert.doesNotMatch(text(dialog), /password|密码|User password|Neko image|临时 Key/)
  instance.$destroy()
})

test('clone uses shared Add-room syntax rules but requires an explicit valid name', async () => {
  const calls = []
  const instance = list(true, async (...args) => calls.push(args))
  for (const name of ['', 'a', '_room', '.room', '-room', 'bad room', 'room/else', 'room中文']) {
    prepareClone(instance, name)
    await instance.Clone()
    assert.equal(calls.length, 0, name)
    assert.equal(instance.cloneLoading, false)
    assert.ok(instance.cloneNameRules.some(rule => rule(name) !== true))
  }
  for (const name of ['ab', 'room_copy-2.0', 'A1']) {
    assert.ok(instance.cloneNameRules.every(rule => rule(name) === true), name)
  }
  assert.ok(nameRules.roomNameRules.every(rule => rule('') === true), 'Add room retains optional random names')
  const create = compiler.parseComponent(readFileSync(resolve(root, 'src/components/RoomsCreate.vue'), 'utf8'))
  assert.match(create.template.content, /:rules="roomNameRules"/)
  instance.$destroy()
})

test('cancel and dismiss before submission have no API effect or source mutation', () => {
  const calls = [], before = plain(rooms)
  const instance = list(true, async (...args) => calls.push(args))
  for (const close of [() => instance.CloseClone(), () => instance.onCloneDialogInput(false)]) {
    prepareClone(instance)
    instance.cloneError = 'previous failure'
    close()
    assert.equal(instance.cloneSource, null)
    assert.equal(instance.cloneError, '')
    assert.equal(instance.cloneName, '')
  }
  assert.deepEqual(calls, [])
  assert.deepEqual(plain(rooms), before)
  instance.$destroy()
})

test('pending clone blocks repeat submit, Cancel, Escape, backdrop and switching source', async () => {
  const gate = deferred(), calls = []
  const instance = list(true, async (...args) => { calls.push(args); if (args[0] === 'ROOMS_CLONE') return gate.promise })
  prepareClone(instance)
  const pending = instance.Clone()
  assert.equal(instance.cloneLoading, true)
  await instance.Clone()
  instance.CloseClone()
  instance.onCloneDialogInput(false)
  instance.OpenClone(rooms[1])
  assert.equal(instance.cloneSource, rooms[0])
  assert.equal(calls.length, 1)
  const dialog = cloneDialog(instance)
  assert.equal(dialog.data.attrs.persistent, true)
  const controls = descendants(dialog).filter(node => ['v-text-field', 'v-btn'].includes(node.tag))
  assert.equal(controls.length, 3)
  assert.ok(controls.every(node => node.data.attrs.disabled))
  gate.resolve({ id: 'new-id' })
  await pending
  assert.equal(instance.cloneLoading, false)
  assert.equal(instance.cloneSource, null)
  assert.deepEqual(plain(calls), [['ROOMS_CLONE', { roomId: rooms[0].id, name: 'new-room' }], ['ROOMS_LOAD']])
  assert.match(instance.cloneSuccess, /new-room.*已创建并启动/)
  assert.equal(instance.cloneError, '')
  instance.$destroy()
})

test('native form submission dispatches once and retains the same table instance/list selection', async () => {
  const calls = []
  const instance = list(true, async (...args) => { calls.push(args); return { id: 'new-id' } })
  prepareClone(instance, 'new-room-with-Enter')
  const form = descendants(cloneDialog(instance)).find(node => node.tag === 'v-form')
  let prevented = 0
  await form.data.on.submit({ preventDefault: () => prevented++ })
  assert.equal(prevented, 1)
  assert.deepEqual(plain(calls), [['ROOMS_CLONE', { roomId: rooms[0].id, name: 'new-room-with-Enter' }], ['ROOMS_LOAD']])
  assert.equal(instance.rooms, rooms)
  assert.equal(instance.cloneSource, null)
  assert.equal(instance.agentRoomName, '')
  instance.$destroy()
})

test('409 including empty bodies stays inline, preserves exact name and never silently renames', async () => {
  for (const data of ['', {}, { message: 'target storage exists' }, 'name already exists']) {
    const calls = []
    const instance = list(true, async (...args) => {
      calls.push(args)
      throw { isAxiosError: true, response: { status: 409, data } }
    })
    prepareClone(instance, 'existing-name')
    await instance.Clone()
    assert.equal(instance.cloneName, 'existing-name')
    assert.equal(instance.cloneSource, rooms[0])
    assert.equal(instance.cloneLoading, false)
    assert.match(instance.cloneError, /名称.*已存在.*HTTP 409/)
    assert.equal(calls.length, 1)
    assert.equal(instance.cloneSuccess, '')
    const error = descendants(cloneDialog(instance)).find(node => node.tag === 'v-alert')
    assert.equal(error.data.attrs.role, 'alert')
    assert.match(text(error), /HTTP 409/)
    const field = descendants(cloneDialog(instance)).find(node => node.tag === 'v-text-field')
    field.data.on.input('another-name')
    assert.equal(instance.cloneError, '')
    instance.$destroy()
  }
})

test('server and network failures stay inline; lost response is not described as rollback', async () => {
  for (const [error, expected] of [
    [{ isAxiosError: true, response: { status: 400, data: { message: 'unmanaged writable mount' } } }, /HTTP 400.*unmanaged writable mount/],
    [{ isAxiosError: true, response: { status: 500, data: '' } }, /HTTP 500/],
    [new Error('Network Error'), /结果尚未确认.*刷新房间列表/],
  ]) {
    const instance = list(true, async () => { throw error })
    prepareClone(instance)
    await instance.Clone()
    assert.match(instance.cloneError, expected)
    assert.equal(instance.cloneLoading, false)
    assert.equal(instance.cloneSource, rooms[0])
    assert.equal(instance.cloneSuccess, '')
    instance.$destroy()
  }
})

test('refresh failure after creation keeps committed success, without offering another POST', async () => {
  const calls = []
  const instance = list(true, async (...args) => {
    calls.push(args)
    if (args[0] === 'ROOMS_LOAD') throw new Error('refresh failed')
    return { id: 'committed-room' }
  })
  prepareClone(instance)
  await instance.Clone()
  assert.equal(instance.cloneSource, null)
  assert.equal(instance.cloneLoading, false)
  assert.equal(instance.cloneError, '')
  assert.match(instance.cloneSuccess, /已创建并启动/)
  assert.match(instance.cloneRefreshError, /已创建.*刷新失败.*不要重复复制/)
  await instance.Clone()
  assert.equal(calls.length, 2)
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
