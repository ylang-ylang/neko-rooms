const PASSWORD_ALPHABET = '0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ'
export const DEFAULT_PASSWORD_LENGTH = 24

/** Generate a URL-safe CSPRNG password without modulo bias (~142.9 bits at 24 chars). */
export function randomPassword(length = DEFAULT_PASSWORD_LENGTH): string {
  if (!Number.isInteger(length) || length <= 0) {
    throw new Error('Password length must be a positive integer')
  }
  const cryptoObj = typeof globalThis !== 'undefined' ? globalThis.crypto : undefined
  if (!cryptoObj || typeof cryptoObj.getRandomValues !== 'function') {
    throw new Error('Cryptographically secure random generator is unavailable')
  }
  const alphabetLen = PASSWORD_ALPHABET.length
  const maxUnbiased = Math.floor(256 / alphabetLen) * alphabetLen
  const buf = new Uint8Array(Math.min(Math.max(length * 2, 32), 65536))
  let out = ''
  while (out.length < length) {
    cryptoObj.getRandomValues(buf)
    for (let i = 0; i < buf.length && out.length < length; i++) {
      if (buf[i] < maxUnbiased) {
        out += PASSWORD_ALPHABET[buf[i] % alphabetLen]
      }
    }
  }
  return out
}
