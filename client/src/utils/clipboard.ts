/** Select the visible prompt first so manual copy remains available on HTTP LAN origins. */
export function selectPrompt(textarea: HTMLTextAreaElement): void {
  textarea.focus()
  textarea.select()
  textarea.setSelectionRange(0, textarea.value.length)
}

export async function copyPrompt(textarea: HTMLTextAreaElement): Promise<boolean> {
  selectPrompt(textarea)

  // Do this before any await: the re-copy button's user gesture must reach execCommand.
  // Unlike navigator.clipboard, this also works on ordinary HTTP origins.
  try {
    if (document.execCommand('copy')) return true
  } catch {
    // Some browsers have removed execCommand; try the modern API below.
  }

  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(textarea.value)
      return true
    }
  } catch {
    // Leave the textarea selected and tell the caller to offer manual copy.
  }
  return false
}
