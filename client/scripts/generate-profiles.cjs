#!/usr/bin/env node
// Bounded OpenAPI sync: reuse the installed SDK's transports, preserving Agent/auth code.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const yaml = require('js-yaml')
const root = path.resolve(__dirname, '../..')
const spec = yaml.load(fs.readFileSync(path.join(root, 'OpenApi.yml'), 'utf8'))
const target = path.join(root, 'client/src/api/api.ts')
const previous = fs.readFileSync(target, 'utf8')
let output = previous
const pattern = (name, indent, suffix = ',') => new RegExp(
  ` {${indent}}/\\*\\*\\n(?:(?!\\*/)[\\s\\S])*\\*/\\n {${indent}}(?:async |public )?${name}[^\\n]*\\n[\\s\\S]*?\\n {${indent}}\\}${suffix}`, 'g')
for (const name of ['BrowserProfile', 'ProfileCreateRequest']) {
  const schema = spec.components.schemas[name]
  const properties = Object.entries(schema.properties).map(([key, value]) => {
    assert.equal(value.type, 'string')
    return `    /** ${value.description || key} */\n    '${key}'${schema.required.includes(key) ? '' : '?'}: string;`
  }).join('\n')
  output = output.replace(new RegExp(`export interface ${name} \\{[\\s\\S]*?\\n\\}\\n`), '')
  const anchor = '/**\n * \n * @export\n * @interface BrowserPolicy\n'
  output = output.replace(anchor, `export interface ${name} {\n${properties}\n}\n` + anchor)
}
for (const [model, fields] of [['RoomEntry', ['profile_id', 'profile_name']], ['RoomSettings', ['profile_id', 'profile_name', 'clone_profile']]]) {
  output = output.replace(new RegExp(`(export interface ${model} \\{\\n)([\\s\\S]*?)(\\n\\})`), (_, start, body, end) => {
    for (const field of fields) {
      body = body.replace(new RegExp(`    /\\*\\* [^\\n]* \\*/\\n    '${field}'\\?: (?:string|boolean);\\n?`, 'g'), '')
    }
    const properties = fields.map(field => {
      const schema = spec.components.schemas[model].properties[field]
      const tsType = schema.type === 'boolean' ? 'boolean' : 'string'
      return `    /** ${schema.description} */\n    '${field}'?: ${tsType};\n`
    }).join('')
    return start + body.trimEnd() + '\n' + properties.trimEnd() + end
  })
}
for (const name of ['profilesList', 'profileCreate', 'profileDelete']) {
  for (const [indent, suffix] of [[8, ','], [4, '']]) output = output.replace(new RegExp(pattern(name, indent, suffix).source + '\n', 'g'), '')
}
for (const [name, template, route, verb] of [
  ['profilesList', 'roomsList', '/api/profiles', 'get'],
  ['profileCreate', 'roomCreate', '/api/profiles', 'post'],
  ['profileDelete', 'roomRemove', '/api/profiles/{profileId}', 'delete'],
]) {
  const operation = spec.paths[route][verb]
  assert.equal(operation.operationId, name)
  for (const [indent, suffix, count] of [[8, ',', 3], [4, '', 1]]) {
    output = output.replace(new RegExp(pattern(name, indent, suffix).source + '\n', 'g'), '')
    const templates = [...output.matchAll(pattern(template, indent, suffix))]
    const anchors = [...output.matchAll(pattern('roomsList', indent, suffix))]
    assert.equal(templates.length, count)
    for (const [index, match] of templates.entries()) {
      let method = match[0].replaceAll(template, name)
        .replaceAll('RoomEntry', 'BrowserProfile').replaceAll('RoomSettings', 'ProfileCreateRequest')
        .replaceAll('roomSettings', 'profileCreateRequest').replaceAll('roomId', 'profileId')
        .replaceAll('/api/rooms', '/api/profiles')
        .replace(/@summary [^\n]*/, `@summary ${operation.summary}`)
        .replace(/^ +\* @param \{boolean\} \[start\].*\n/m, '')
        .replaceAll('start?: boolean, ', '').replaceAll(', start, options', ', options')
        .replace(/\n {12}if \(start !== undefined\) \{\n {16}localVarQueryParameter\['start'\] = start;\n {12}\}\n/, '')
      if (name === 'profilesList') {
        method = method.replace(/^ +\* @param .*\[labels\].*\n/m, '')
          .replaceAll('labels?: { [key: string]: string; }, ', '').replaceAll('labels, options', 'options')
          .replace(/\n {12}if \(labels !== undefined\) \{[\s\S]*?\n {12}\}\n/, '')
      }
      output = output.replace(anchors[index][0], method.replace(/[ \t]+$/gm, '') + '\n' + anchors[index][0])
    }
  }
}
if (process.argv.includes('--check')) assert.equal(output, previous, 'profile SDK is stale; run node scripts/generate-profiles.cjs')
else if (output !== previous) fs.writeFileSync(target, output)
