import assert from "node:assert/strict";
import { test } from "node:test";

import { isInput } from "../src/input.ts";

test("isInput accepts the Queue page's pointer and wheel events", () => {
  for (const action of ["down", "move", "up"]) {
    assert.ok(isInput({ type: "pointer", action, x: 0, y: 1, t: 12.5 }), action);
  }
  assert.ok(isInput({ type: "wheel", x: 0.5, y: 0.5, dx: -10, dy: 0.25, t: 1 }));
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
    null,
    "pointer",
  ]) {
    assert.equal(isInput(bad), false, JSON.stringify(bad));
  }
});
