import assert from "node:assert/strict";
import { test } from "node:test";

import { isInput } from "../src/input.ts";

test("isInput accepts the Queue page's pointer and wheel events", () => {
  for (const action of ["down", "move", "up"]) {
    assert.ok(isInput({ type: "pointer", action, x: 0, y: 1, t: 12.5 }), action);
  }
  assert.ok(isInput({ type: "wheel", x: 0.5, y: 0.5, dx: -10, dy: 0.25, t: 1 }));
});

test("isInput accepts typed text and allowed keys", () => {
  assert.ok(isInput({ type: "text", text: "Eli Lilly", t: 1 }));
  assert.ok(isInput({ type: "text", text: "é".repeat(64), t: 1 }));
  for (const key of ["Enter", "Backspace", "Tab", "ArrowLeft", "Escape"]) {
    assert.ok(isInput({ type: "key", key, t: 1 }), key);
  }
});

test("isInput refuses what the backend would refuse", () => {
  for (const bad of [
    { type: "pointer", action: "tap", x: 0.5, y: 0.5 },
    { type: "pointer", action: "down", x: 1.5, y: 0.5 },
    { type: "pointer", action: "down", x: 0.5, y: -0.1 },
    { type: "pointer", action: "down", x: "0.5", y: 0.5 },
    { type: "pointer", action: "down", x: Number.NaN, y: 0.5 },
    { type: "wheel", x: 0.5, y: 0.5, dx: 0, dy: 50 },
    { type: "wheel", x: 0.5, y: 0.5, dx: 0 },
    { type: "keyboard", key: "a" },
    { type: "text", text: "" },
    { type: "text", text: "a".repeat(65) },
    { type: "text", text: "a\nb" },
    { type: "text", text: 7 },
    { type: "key", key: "Meta" },
    { type: "key", key: "Control+L" },
    { type: "key", key: "a" },
    null,
    "pointer",
  ]) {
    assert.equal(isInput(bad), false, JSON.stringify(bad));
  }
});
