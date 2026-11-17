// Placement of the small ⓘ notes on the FinOps pages.

export const NOTE_WIDTH = 256;
export const NOTE_GUTTER = 8;

/**
 * Where the note goes: under the ⓘ, moved left or above so it stays inside
 * the viewport. Fixed coordinates, so a scrolling table or a clipped card
 * cannot cut it off.
 */
export function placeNote(
  anchor: { left: number; top: number; bottom: number },
  noteHeight: number,
  viewport: { width: number; height: number },
  noteWidth: number = NOTE_WIDTH,
): { left: number; top: number; width: number } {
  const width = Math.min(noteWidth, viewport.width - 2 * NOTE_GUTTER);
  const left = Math.max(NOTE_GUTTER, Math.min(anchor.left, viewport.width - NOTE_GUTTER - width));
  let top = anchor.bottom + 4;
  if (top + noteHeight > viewport.height - NOTE_GUTTER && anchor.top - 4 - noteHeight >= NOTE_GUTTER) {
    top = anchor.top - 4 - noteHeight;
  }
  return { left, top, width };
}
