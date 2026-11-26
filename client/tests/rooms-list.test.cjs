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
const nekoImageUtils = {}
runInNewContext(ts.transpileModule(readFileSync(resolve(root, 'src/utils/neko-image.ts'), 'utf8'), {
  compilerOptions: { ...options, module: ts.ModuleKind.CommonJS },
}).outputText, { exports: nekoImageUtils })
const mocks = {
  '@/utils/room-name': nameRules,
  '@/utils/neko-image': nekoImageUtils,
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

test('formatNekoImage compacts long digest-pinned images while preserving full reference', () => {
  const { formatNekoImage } = nekoImageUtils
  assert.deepEqual(plain(formatNekoImage('docker.io/ylangylang/neko-chromium-cdp@sha256:96e413b7a82a0ec6043597151ce5f2183d34188aa81cc692af3365eafa1bd4e2')), {
    name: 'neko-chromium-cdp',
    digest: '@96e413b7',
    short: 'neko-chromium-cdp@96e413b7',
    full: 'docker.io/ylangylang/neko-chromium-cdp@sha256:96e413b7a82a0ec6043597151ce5f2183d34188aa81cc692af3365eafa1bd4e2',
  })
  assert.deepEqual(plain(formatNekoImage(pinnedImage)), {
    name: 'chromium:3.2.0',
    digest: '@270face8',
    short: 'chromium:3.2.0@270face8',
    full: pinnedImage,
  })
  assert.deepEqual(plain(formatNekoImage('m1k1o/neko:chromium')), {
    name: 'neko:chromium',
    digest: '',
    short: 'neko:chromium',
    full: 'm1k1o/neko:chromium',
  })
  assert.deepEqual(plain(formatNekoImage('localhost:5000/org/neko-firefox:dev@sha256:abcdef0123456789')), {
    name: 'neko-firefox:dev',
    digest: '@abcdef01',
    short: 'neko-firefox:dev@abcdef01',
    full: 'localhost:5000/org/neko-firefox:dev@sha256:abcdef0123456789',
  })
  assert.deepEqual(plain(formatNekoImage('sha256:96e413b7a82a0ec6043597151ce5f2183d34188aa81cc692af3365eafa1bd4e2')), {
    name: 'sha256:96e413b7',
    digest: '',
    short: 'sha256:96e413b7',
    full: 'sha256:96e413b7a82a0ec6043597151ce5f2183d34188aa81cc692af3365eafa1bd4e2',
  })
  assert.deepEqual(plain(formatNekoImage('user@host/org/neko:latest')), {
    name: 'neko:latest',
    digest: '',
    short: 'neko:latest',
    full: 'user@host/org/neko:latest',
  })
  assert.deepEqual(plain(formatNekoImage('@sha256:96e413b7a82a0ec6043597151ce5f2183d34188aa81cc692af3365eafa1bd4e2')), {
    name: '@96e413b7',
    digest: '',
    short: '@96e413b7',
    full: '@sha256:96e413b7a82a0ec6043597151ce5f2183d34188aa81cc692af3365eafa1bd4e2',
  })
  assert.deepEqual(plain(formatNekoImage('  ')), { name: '', digest: '', short: '', full: '' })
  assert.deepEqual(plain(formatNekoImage(undefined)), { name: '', digest: '', short: '', full: '' })
})

test('long names stay visible while pinned images display compact name/digest with full tooltip and aria-label', () => {
  const instance = list()
  for (const room of rooms) {
    assert.equal(text(slot(instance, 'name', room)[0]).trim(), room.name)
    const image = slot(instance, 'neko_image', room)[0]
    const [recreate, tooltip] = elements(image.children)
    assert.deepEqual(plain(recreate.componentOptions.propsData), { action: 'recreate', roomId: room.id })
    assert.equal(tooltip.data.attrs.disabled, false)
    const label = elements(tooltip.data.scopedSlots.activator({ on: {}, attrs: {} }))[0]
    assert.equal(label.data.attrs['aria-label'], 'Neko image: ' + pinnedImage)
    assert.equal(label.data.attrs.title, undefined)
    const [nameEl, digestEl] = elements(label.children)
    assert.equal(nameEl.data.staticClass, 'room-image-name')
    assert.equal(text(nameEl), 'chromium:3.2.0')
    assert.equal(digestEl.data.staticClass, 'room-image-digest')
    assert.equal(text(digestEl), '@270face8')
    assert.equal(text(elements(tooltip.children)[0]), pinnedImage)
    assert.equal(text(slot(instance, 'status', room)[0]).trim(), room.status)
    assert.equal(table(instance).data.scopedSlots['item.created']({ item: room })[0].text.trim(), 'relative ' + room.created)
  }
  const plainRoom = { ...rooms[0], neko_image: 'm1k1o/neko:chromium' }
  const [, plainTooltip] = elements(slot(instance, 'neko_image', plainRoom)[0].children)
  const plainLabel = elements(plainTooltip.data.scopedSlots.activator({ on: {}, attrs: {} }))[0]
  assert.equal(plainLabel.data.attrs['aria-label'], 'Neko image: m1k1o/neko:chromium')
  assert.equal(elements(plainLabel.children).length, 1)
  assert.equal(text(elements(plainLabel.children)[0]), 'neko:chromium')
  assert.equal(text(elements(plainTooltip.children)[0]), 'm1k1o/neko:chromium')
  const emptyRoom = { ...rooms[0], neko_image: '' }
  const [, emptyTooltip] = elements(slot(instance, 'neko_image', emptyRoom)[0].children)
  assert.equal(emptyTooltip.data.attrs.disabled, true)
  instance.$destroy()
})

test('profile names use an icon chip below the room title with a full-name tooltip', () => {
  const instance = list()
  const room = { ...rooms[0], profile_id: 'independent-profile', profile_name: 'Long account name '.repeat(8) }
  const [title, tooltip] = elements(slot(instance, 'name', room)[0].children)
  assert.equal(title.data.staticClass, 'room-title')
  assert.equal(text(title), room.name)
  const chip = elements(tooltip.data.scopedSlots.activator({ on: {}, attrs: {} }))[0]
  assert.equal(chip.data.staticClass, 'room-profile')
  assert.equal(chip.data.attrs['aria-label'], 'Browser profile: ' + room.profile_name)
  const [icon, label] = elements(chip.children)
  assert.equal(text(icon), 'mdi-account-circle-outline')
  assert.equal(text(label), room.profile_name)
  assert.equal(text(elements(tooltip.children)[0]), room.profile_name + ' (' + room.profile_id + ')')
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
    assert.equal(clone.data.attrs['aria-label'], 'Clone deployment ' + item.name)
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
  assert.equal(fields[0].data.attrs.label, 'New deployment name')
  assert.equal(fields[0].data.attrs.autofocus, '')
  assert.equal(fields[0].data.attrs.autocomplete, 'off')
  let selected = 0
  fields[0].data.on.focus({ target: { select: () => selected++ } })
  assert.equal(selected, 1)
  assert.equal(nodes.filter(node => node.tag === 'v-form').length, 1)
  assert.equal(typeof nodes.find(node => node.tag === 'v-form').data.on.submit, 'function')
  assert.equal(nodes.find(node => node.tag === 'v-btn' && text(node) === 'Clone and start').data.attrs.type, 'submit')
  assert.doesNotMatch(text(dialog), /password|User password|Neko image|Temporary key/)
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
  assert.match(instance.cloneSuccess, /new-room.*created and started/)
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
    assert.match(instance.cloneError, /name already exists.*HTTP 409/)
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
    [new Error('Network Error'), /outcome is unconfirmed.*Refresh the deployment list/],
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
  assert.match(instance.cloneSuccess, /created and started/)
  assert.match(instance.cloneRefreshError, /created.*refresh failed.*do not clone again/)
  await instance.Clone()
  assert.equal(calls.length, 2)
  instance.$destroy()
})

test('optional connections use compact desktop actions without shrinking targets or changing mux/mobile layout', () => {
  for (const usesMux of [true, false]) {
    const instance = list(usesMux)
    assert.equal(slot(instance, 'actions')[0].data.class['room-actions--compact'], !usesMux)
    instance.$destroy()
  }
  assert.match(source.styles[0].content, /@media \(min-width: 600px\)\s*\{[\s\S]*\.room-actions--compact\s*\{[^}]*grid-template-columns: repeat\(3, 36px\);[^}]*width: max-content;/)
})

test('layout rules are scoped, compact image cells cleanly and leave mux action buttons on one line', () => {
  assert.deepEqual(compiler.compile(source.template.content).errors, [])
  assert.equal(source.styles.length, 1)
  assert.equal(source.styles[0].scoped, true)
  const style = source.styles[0].content
  assert.match(style, /\.room-name\s*\{[^}]*min-width:[^}]*overflow-wrap: anywhere/s)
  assert.match(style, /\.room-image\s*\{[^}]*max-width:\s*min\(13\.5rem,\s*100%\)/s)
  assert.match(style, /@media \(min-width: 600px\)\s*\{[\s\S]*\.room-image\s*\{[^}]*min-width:\s*9\.5rem/s)
  assert.match(style, /\.room-image-label\s*\{[^}]*text-align:\s*left/s)
  assert.match(style, /\.room-image-name\s*\{[^}]*overflow: hidden;[^}]*text-overflow: ellipsis;[^}]*white-space: nowrap/s)
  assert.match(style, /\.room-image-digest\s*\{[^}]*overflow: hidden;[^}]*text-overflow: ellipsis;[^}]*white-space: nowrap/s)
  assert.match(style, /\.room-image-tooltip\s*\{[^}]*white-space: normal;[^}]*overflow-wrap: anywhere/s)
  assert.match(style, /\.room-actions\s*\{[^}]*white-space: nowrap/s)
  assert.doesNotMatch(style, /table-layout:\s*fixed/)
  assert.doesNotMatch(style.match(/\.room-name\s*\{([^}]*)\}/)[1], /overflow:\s*hidden/)
  assert.match(style, /\.room-profile-name\s*\{[^}]*overflow: hidden;[^}]*text-overflow: ellipsis;[^}]*white-space: nowrap/s)
})
