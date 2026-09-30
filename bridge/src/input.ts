// The Solver's input events, as the Queue page sends them. Positions are
// normalized to 0–1 of the displayed frame; a wheel's scroll is in frame
// widths and heights.

export type Pointer = { type: "pointer"; action: "down" | "move" | "up"; x: number; y: number; t: number };
export type Wheel = { type: "wheel"; x: number; y: number; dx: number; dy: number; t: number };
export type Input = Pointer | Wheel;

/** Bounds one wheel event's scroll, in frame widths or heights, as the backend does. */
const MAX_WHEEL = 10;

/**
 * Reports whether m is a well-formed input event. The backend checks input
 * it relays; input from a direct peer reaches the Bridge unchecked.
 */
export function isInput(m: unknown): m is Input {
  if (typeof m !== "object" || m === null) return false;
  const e = m as Record<string, unknown>;
  const inFrame = (v: unknown) => typeof v === "number" && v >= 0 && v <= 1;
  const scroll = (v: unknown) => typeof v === "number" && Math.abs(v) <= MAX_WHEEL;
  if (!inFrame(e.x) || !inFrame(e.y)) return false;
  if (e.type === "pointer") return e.action === "down" || e.action === "move" || e.action === "up";
  return e.type === "wheel" && scroll(e.dx) && scroll(e.dy);
}
