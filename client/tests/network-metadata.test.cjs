const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const { spawnSync } = require('node:child_process')
const { test } = require('node:test')
const ts = require('typescript')
const yaml = require('js-yaml')

const root = path.resolve(__dirname, '../..')

test('network metadata is optional, schema-aligned and read-only only in room settings', () => {
  const document = yaml.load(fs.readFileSync(path.join(root, 'OpenApi.yml'), 'utf8'))
  const file = ts.createSourceFile('api.ts', fs.readFileSync(path.join(root, 'client/src/api/api.ts'), 'utf8'), ts.ScriptTarget.Latest, true)
  for (const name of ['RoomsConfig', 'RoomEntry', 'RoomSettings']) {
    const model = file.statements.find(node => ts.isInterfaceDeclaration(node) && node.name.text === name)
    const property = model.members.find(node => node.name && node.name.text === 'network_mode')
    const schema = document.components.schemas[name].properties.network_mode
    assert.ok(property && property.questionToken, name + ' metadata must remain optional')
    assert.ok(ts.isUnionTypeNode(property.type))
    assert.deepEqual(property.type.types.map(node => node.literal.text), schema.enum)
    assert.equal(Boolean(property.modifiers?.some(node => node.kind === ts.SyntaxKind.ReadonlyKeyword)), schema.readOnly === true)
  }
})

test('bounded network metadata generation is reproducible without replacing the SDK', () => {
  const result = spawnSync(process.execPath, [path.join(root, 'dev/api-gen-network-types.cjs'), '--check'], { encoding: 'utf8' })
  assert.equal(result.status, 0, result.stderr || result.stdout)
})
