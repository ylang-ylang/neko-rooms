const assert = require('node:assert/strict')
const { webcrypto } = require('node:crypto')
const { readFileSync } = require('node:fs')
const { resolve } = require('node:path')
const { test } = require('node:test')
const { runInNewContext } = require('node:vm')
const ts = require('typescript')

const root = resolve(__dirname, '..')
const config = ts.readConfigFile(resolve(root, 'tsconfig.json'), ts.sys.readFile).config
const options = ts.convertCompilerOptionsFromJson(config.compilerOptions, root).options
const transpiled = ts.transpileModule(readFileSync(resolve(root, 'src/utils/random.ts'), 'utf8'), {
  compilerOptions: { ...options, module: ts.ModuleKind.CommonJS },
}).outputText

function loadRandom(customCrypto) {
  const exportsObject = {}
  const ctx = {
    exports: exportsObject,
    Uint8Array,
    Math: Object.assign(Object.create(Math), {
      random() {
        throw new Error('Math.random must not be used for room passwords')
      },
    }),
  }
  ctx.globalThis = { crypto: customCrypto }
  runInNewContext(transpiled, ctx)
  return exportsObject
}

test('randomPassword generates 24-char alphanumeric CSPRNG passwords without Math.random', () => {
  const { randomPassword, DEFAULT_PASSWORD_LENGTH } = loadRandom(webcrypto)
  assert.equal(DEFAULT_PASSWORD_LENGTH, 24)
  const seen = new Set()
  for (let i = 0; i < 32; i++) {
    const pw = randomPassword()
    assert.equal(pw.length, 24)
    assert.match(pw, /^[0-9a-zA-Z]{24}$/)
    seen.add(pw)
  }
  assert.equal(seen.size, 32)
})

test('randomPassword uses unbiased rejection sampling for bytes >= 248', () => {
  // Alphabet length is 62; floor(256/62)*62 = 248. Bytes 248..255 must be skipped.
  const sequence = [248, 249, 250, 255, 0, 1, 61, 62, 123, 247]
  let offset = 0
  const { randomPassword } = loadRandom({
    getRandomValues(arr) {
      for (let i = 0; i < arr.length; i++) {
        arr[i] = sequence[offset++ % sequence.length]
      }
      return arr
    },
  })
  // Accepted values: 0 -> '0', 1 -> '1', 61 -> 'Z', 62%62=0 -> '0', 123%62=61 -> 'Z', 247%62=61 -> 'Z'
  assert.equal(randomPassword(6), '01Z0ZZ')
})

test('randomPassword fails closed when Web Crypto API is unavailable', () => {
  const { randomPassword } = loadRandom(undefined)
  assert.throws(() => randomPassword(), /Cryptographically secure random generator is unavailable/)
})
