export interface CompactNekoImage {
  /** Short repository name (+ optional :tag), without registry/namespace prefix or full digest. */
  name: string;
  /** Shortened digest badge (e.g. "@96e413b7") when the image reference is digest-pinned. */
  digest: string;
  /** Single-line compact label combining name and short digest (e.g. "neko-chromium-cdp@96e413b7"). */
  short: string;
  /** Original verbatim image reference. */
  full: string;
}

const SHORT_DIGEST_LEN = 8

export function formatNekoImage(raw?: string | null): CompactNekoImage {
  const full = typeof raw === 'string' ? raw.trim() : ''
  if (!full) {
    return { name: '', digest: '', short: '', full: '' }
  }

  let ref = full
  let digest = ''
  const lastSlashInFull = full.lastIndexOf('/')
  const atIndex = full.lastIndexOf('@')
  if (atIndex !== -1 && atIndex > lastSlashInFull) {
    ref = full.slice(0, atIndex)
    const rawDigest = full.slice(atIndex + 1)
    const colonIndex = rawDigest.indexOf(':')
    const value = colonIndex !== -1 ? rawDigest.slice(colonIndex + 1) : rawDigest
    if (value) {
      digest = '@' + value.slice(0, SHORT_DIGEST_LEN)
    }
    if (!ref) {
      const onlyDigest = digest || full
      return { name: onlyDigest, digest: '', short: onlyDigest, full }
    }
  } else if (/^sha(?:256|512):[a-fA-F0-9]{12,}$/.test(full)) {
    const colonIndex = full.indexOf(':')
    const shortId = full.slice(0, colonIndex + 1 + SHORT_DIGEST_LEN)
    return { name: shortId, digest: '', short: shortId, full }
  }

  const lastSlash = ref.lastIndexOf('/')
  const name = lastSlash !== -1 ? ref.slice(lastSlash + 1) : ref
  const displayName = name || ref || full
  const short = digest ? `${displayName}${digest}` : displayName

  return {
    name: displayName,
    digest,
    short,
    full,
  }
}
