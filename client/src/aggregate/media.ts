// Bounded native MSE plumbing; buffers are real muxed bytes, never decoded or fabricated here.
export const MEDIA_LIMITS = { bytes: 8 * 1024 * 1024, chunks: 512, keepSeconds: 10, maxLagSeconds: 1.5, operationMs: 15000 }

export class DesktopMedia {
  private media: MediaSource | null = null
  private buffer: SourceBuffer | null = null
  private url: string | null = null
  private queue: ArrayBuffer[] = []
  private queuedBytes = 0
  private activeBytes = 0
  private timer: ReturnType<typeof setTimeout> | null = null
  private mime = ''
  private stopped = true

  constructor(private video: HTMLVideoElement, private onError: (message: string) => void, private onPlayRequired: () => void) {}

  start(mime: string): void {
    this.stop()
    if (typeof MediaSource === 'undefined' || !MediaSource.isTypeSupported(mime)) {
      this.onError('This browser cannot play the server media type: ' + mime + '. Use a browser with compatible MediaSource support.')
      return
    }
    this.stopped = false
    this.mime = mime
    try {
      this.media = new MediaSource()
      this.media.addEventListener('sourceopen', this.open)
      this.media.addEventListener('sourceclose', this.closed)
      this.video.addEventListener('error', this.decodeError)
      this.video.addEventListener('canplay', this.play)
      this.video.addEventListener('timeupdate', this.maintain)
      this.url = URL.createObjectURL(this.media)
      this.video.src = this.url
      this.armTimeout()
    } catch {
      this.fail('Could not initialize desktop media playback. Reconnect to try again.')
    }
  }

  push(bytes: ArrayBuffer): void {
    if (this.stopped) return
    if (!bytes.byteLength) return
    if (this.queuedBytes + this.activeBytes + bytes.byteLength > MEDIA_LIMITS.bytes ||
        this.queue.length + (this.activeBytes ? 1 : 0) >= MEDIA_LIMITS.chunks) {
      // WebM deltas cannot be dropped arbitrarily. A fresh authenticated stream/init is required.
      this.fail('Desktop media queue is full. Reconnect for a fresh live stream.')
      return
    }
    this.queue.push(bytes)
    this.queuedBytes += bytes.byteLength
    this.flush()
  }

  private open = (): void => {
    if (this.stopped || !this.media || this.buffer) return
    this.clearTimer()
    try {
      this.buffer = this.media.addSourceBuffer(this.mime)
      this.buffer.addEventListener('updateend', this.updated)
      this.buffer.addEventListener('error', this.decodeError)
      this.buffer.addEventListener('abort', this.aborted)
      this.flush()
    } catch {
      this.fail('The browser rejected the server media configuration. Reconnect or use a compatible browser.')
    }
  }

  private updated = (): void => {
    this.activeBytes = 0
    this.clearTimer()
    this.flush()
  }

  private flush(): void {
    const buffer = this.buffer
    if (this.stopped || !buffer || buffer.updating) return
    try {
      this.maintain()
      const ranges = buffer.buffered
      if (ranges.length) {
        const cutoff = ranges.end(ranges.length - 1) - MEDIA_LIMITS.keepSeconds
        if (cutoff > ranges.start(0) + 0.5) {
          this.armTimeout()
          buffer.remove(0, cutoff)
          return // updateend serializes removal and subsequent appends.
        }
      }
      const bytes = this.queue.shift()
      if (!bytes) return
      this.queuedBytes -= bytes.byteLength
      this.activeBytes = bytes.byteLength
      this.armTimeout()
      buffer.appendBuffer(bytes)
    } catch {
      this.fail('The desktop media buffer failed or filled up. Reconnect for a fresh stream.')
    }
  }

  private maintain = (): void => {
    const ranges = this.buffer?.buffered
    if (!ranges?.length || this.video.readyState === 0) return
    const start = ranges.start(ranges.length - 1), end = ranges.end(ranges.length - 1)
    if (this.video.currentTime < start || end - this.video.currentTime > MEDIA_LIMITS.maxLagSeconds || this.video.currentTime > end) {
      this.video.currentTime = Math.max(start, end - 0.25)
    }
  }

  private play = (): void => {
    this.maintain()
    this.video.play().catch(() => { if (!this.stopped) this.onPlayRequired() })
  }
  private closed = (): void => { this.fail('The desktop media source closed. Reconnect to resume.') }
  private decodeError = (): void => { this.fail('Desktop media could not be decoded. Reconnect or use a compatible browser.') }
  private aborted = (): void => { this.fail('Desktop media append was interrupted. Reconnect to resume.') }
  private armTimeout(): void {
    this.clearTimer()
    this.timer = setTimeout(() => this.fail('Desktop media stalled. Reconnect for a fresh stream.'), MEDIA_LIMITS.operationMs)
  }
  private clearTimer(): void {
    if (this.timer !== null) clearTimeout(this.timer)
    this.timer = null
  }
  private fail(message: string): void {
    if (this.stopped) return
    this.stop()
    this.onError(message)
  }

  stop(): void {
    this.stopped = true
    this.clearTimer()
    this.queue = []
    this.queuedBytes = 0
    this.activeBytes = 0
    const media = this.media, buffer = this.buffer
    this.media = null
    this.buffer = null
    media?.removeEventListener('sourceopen', this.open)
    media?.removeEventListener('sourceclose', this.closed)
    if (buffer) {
      buffer.removeEventListener('updateend', this.updated)
      buffer.removeEventListener('error', this.decodeError)
      buffer.removeEventListener('abort', this.aborted)
      try {
        if (media?.readyState === 'open') {
          if (buffer.updating) buffer.abort()
          media.removeSourceBuffer(buffer)
        }
      } catch { /* Detaching the video below also releases a closing/failed MediaSource. */ }
    }
    this.video.removeEventListener('error', this.decodeError)
    this.video.removeEventListener('canplay', this.play)
    this.video.removeEventListener('timeupdate', this.maintain)
    this.video.pause()
    this.video.removeAttribute('src')
    this.video.load()
    if (this.url) URL.revokeObjectURL(this.url)
    this.url = null
  }
}
