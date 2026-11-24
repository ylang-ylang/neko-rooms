#!/usr/bin/env node
// Bounded schema-driven sync for network_mode metadata; leaves API transports,
// auth handling and handwritten Agent code untouched. Uses installed client dev tooling.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const yaml = require('../client/node_modules/js-yaml')

const root = path.resolve(__dirname, '..')
const document = yaml.load(fs.readFileSync(path.join(root, 'OpenApi.yml'), 'utf8'))
const target = path.join(root, 'client/src/api/api.ts')
const previous = fs.readFileSync(target, 'utf8')
let output = previous
for (const model of ['RoomsConfig', 'RoomEntry', 'RoomSettings']) {
  const schema = document.components.schemas[model].properties.network_mode
  assert.equal(schema.type, 'string')
  assert.ok(schema.enum.every(value => typeof value === 'string'))
  const type = schema.enum.map(value => JSON.stringify(value).replaceAll('"', "'")).join(' | ')
  const description = schema.description.replace(/\s+/g, ' ').trim()
  const property = `    /**\n     * ${description}\n     * @type {string}\n     * @memberof ${model}\n     */\n    ${schema.readOnly ? 'readonly ' : ''}'network_mode'?: ${type};\n`
  const pattern = new RegExp(`(export interface ${model} \\{\\n)([\\s\\S]*?)(\\n\\})`)
  assert.ok(pattern.test(output), `missing generated interface ${model}`)
  output = output.replace(pattern, (_, start, body, end) => {
    const withoutMetadata = body.replace(/ {4}\/\*\*(?:(?!\*\/)[\s\S])*\*\/\n {4}(?:readonly )?'network_mode'\?:[^\n]*\n?/g, '')
    return start + property + withoutMetadata + end
  })
}
if (process.argv.includes('--check')) {
  assert.equal(output, previous, 'network metadata is stale; run node dev/api-gen-network-types.cjs')
} else if (output !== previous) {
  fs.writeFileSync(target, output)
}
