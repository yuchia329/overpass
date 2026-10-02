// The Solver's input events, as the Queue page sends them. Positions are
// normalized to 0–1 of the displayed frame; a wheel's scroll is in frame
// widths and heights. Text is typed characters; a key is a named key press.

export type Pointer = { type: "pointer"; action: "down" | "move" | "up"; x: number; y: number; t: number };
export type Wheel = { type: "wheel"; x: number; y: number; dx: number; dy: number; t: number };
export type Text = { type: "text"; text: string; t: number };
export type Key = { type: "key"; key: string; t: number };
export type Input = Pointer | Wheel | Text | Key;

/** Bounds one wheel event's scroll, in frame widths or heights, as the backend does. */
const MAX_WHEEL = 10;

/** Bounds the characters one text event types, as the backend does. */
const MAX_TEXT = 64;

/**
 * The named keys a Solver may press, as the backend allows. Modifiers are
 * left out, so a Solver cannot send shortcuts such as Ctrl+L.
 */
const KEYS = new Set([
  "Enter", "Tab", "Backspace", "Delete", "Escape",
  "ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown",
  "Home", "End", "PageUp", "PageDown",
]);

// Control characters are refused in text: Enter and Tab are keys.
const isText = (v: unknown) =>
  typeof v === "string" && v.length > 0 && [...v].length <= MAX_TEXT && !/\p{Cc}/u.test(v);

/**
 * Reports whether m is a well-formed input event. The backend checks input
 * it relays; input from a direct peer reaches the Bridge unchecked.
 */
export function isInput(m: unknown): m is Input {
  if (typeof m !== "object" || m === null) return false;
  const e = m as Record<string, unknown>;
  const inFrame = (v: unknown) => typeof v === "number" && v >= 0 && v <= 1;
  const scroll = (v: unknown) => typeof v === "number" && Math.abs(v) <= MAX_WHEEL;
  if (e.type === "text") return isText(e.text);
  if (e.type === "key") return typeof e.key === "string" && KEYS.has(e.key);
  if (!inFrame(e.x) || !inFrame(e.y)) return false;
  if (e.type === "pointer") return e.action === "down" || e.action === "move" || e.action === "up";
  return e.type === "wheel" && scroll(e.dx) && scroll(e.dy);
}
