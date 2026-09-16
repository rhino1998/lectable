// Distinguishes a plain click from the click that ends a click-and-drag
// text selection - a browser still fires a click event after a drag
// selection, and without this guard that click would also trigger
// whatever click-to-play/click-to-seek handler sits on the same text,
// interrupting the reader's attempt to just copy a line mid-selection.
export function hasActiveTextSelection(): boolean {
  const selection = window.getSelection()
  return !!selection && selection.toString().length > 0
}
