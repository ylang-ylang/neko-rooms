#!/usr/bin/env node
// Bounded OpenAPI sync using the installed SDK's own templates. Do not replace the
// API directory: it also owns handwritten Agent code and network metadata.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const yaml = require('js-yaml')

const root = path.resolve(__dirname, '../..')
const document = yaml.load(fs.readFileSync(path.join(root, 'OpenApi.yml'), 'utf8'))
const route = '/api/rooms/{roomId}/clone'
const operation = document.paths[route].post
const schema = document.components.schemas.RoomCloneRequest
assert.equal(operation.operationId, 'roomClone')
assert.deepEqual(operation.parameters.map(({ name, in: location, required }) => ({ name, location, required })),
  [{ name: 'roomId', location: 'path', required: true }])
assert.equal(operation.requestBody.required, true)
assert.equal(operation.requestBody.content['application/json'].schema.$ref, '#/components/schemas/RoomCloneRequest')
assert.equal(operation.responses['201'].content['application/json'].schema.$ref, '#/components/schemas/RoomEntry')
assert.deepEqual(schema.required, ['name'])
assert.deepEqual(Object.keys(schema.properties), ['name'])
assert.equal(schema.properties.name.type, 'string')
assert.equal(schema.additionalProperties, false)

const target = path.join(root, 'client/src/api/api.ts')
const previous = fs.readFileSync(target, 'utf8')
let output = previous
const methodPattern = (name, indent, suffix = ',') => new RegExp(
  ` {${indent}}/\\*\\*\\n(?:(?!\\*/)[\\s\\S])*\\*/\\n {${indent}}(?:async |public )?${name}[^\\n]*\\n[\\s\\S]*?\\n {${indent}}\\}${suffix}`, 'g')
// Drop only previously emitted clone sections, making this safe to run repeatedly.
for (const [indent, suffix] of [[8, ','], [4, '']]) {
  output = output.replace(new RegExp(methodPattern('roomClone', indent, suffix).source + '\\n', 'g'), '')
}
output = output.replace(/\/\*\*\n \* ?\n \* @export\n \* @interface RoomCloneRequest\n \*\/\nexport interface RoomCloneRequest \{[\s\S]*?\n\}\n/, '')
const model = `/**\n * \n * @export\n * @interface RoomCloneRequest\n */\nexport interface RoomCloneRequest {\n    /**\n     * ${schema.properties.name.description}\n     * @type {string}\n     * @memberof RoomCloneRequest\n     */\n    'name': string;\n}\n`
const modelAnchor = '/**\n * \n * @export\n * @interface RoomEntry\n'
assert.ok(output.includes(modelAnchor))
output = output.replace(modelAnchor, model.replace(/[ \t]+$/gm, '') + modelAnchor)
for (const [indent, suffix, count] of [[8, ',', 3], [4, '', 1]]) {
  const templates = [...output.matchAll(methodPattern('roomRecreate', indent, suffix))]
  const anchors = [...output.matchAll(methodPattern('roomCreate', indent, suffix))]
  assert.equal(templates.length, count)
  assert.equal(anchors.length, count)
  for (let index = 0; index < count; index++) {
    const method = templates[index][0]
      .replaceAll('Recreate room', operation.summary)
      .replaceAll('roomRecreate', 'roomClone')
      .replaceAll('RoomSettings', 'RoomCloneRequest')
      .replaceAll('roomSettings', 'roomCloneRequest')
      .replaceAll('/api/rooms/{roomId}/recreate', route)
      .replace(/^ +\* @param \{boolean\} \[start\] \n/m, '')
      .replaceAll('start?: boolean, ', '')
      .replaceAll('roomCloneRequest, start, options', 'roomCloneRequest, options')
      .replace(/\n {12}if \(start !== undefined\) \{\n {16}localVarQueryParameter\['start'\] = start;\n {12}\}\n/, '')
      .replace(/[ \t]+$/gm, '')
    assert.doesNotMatch(method, /\bstart\??:|\[start\]|\['start'\]|, start,/)
    output = output.replace(anchors[index][0], method + '\n' + anchors[index][0])
  }
}
if (process.argv.includes('--check')) {
  assert.ok(output === previous, 'clone API is stale; run npm run generate:clone')
} else if (output !== previous) {
  fs.writeFileSync(target, output)
}
