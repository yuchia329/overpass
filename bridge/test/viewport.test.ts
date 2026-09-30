import assert from "node:assert/strict";
import { test } from "node:test";

import { toViewport, wheelToViewport } from "../src/viewport.ts";

const desktop = { deviceWidth: 1280, deviceHeight: 800, pageScaleFactor: 1, offsetTop: 0 };
// A non-responsive 1200px-wide page on a 375×667 phone, zoomed out to fit.
const zoomedPhone = { deviceWidth: 375, deviceHeight: 667, pageScaleFactor: 0.3125, offsetTop: 0 };

test("a point on the frame maps to the same point of a desktop viewport", () => {
  assert.deepEqual(toViewport(desktop, 0.5, 0.25), { x: 640, y: 200 });
});

test("a zoomed-out page maps frame points to CSS pixels of the whole layout", () => {
  assert.deepEqual(toViewport(zoomedPhone, 1, 0), { x: 1200, y: 0 });
  assert.deepEqual(toViewport(zoomedPhone, 0.5, 0.3), { x: 600, y: 640.32 });
});

test("the frame's top offset is not part of the page", () => {
  assert.deepEqual(toViewport({ ...desktop, offsetTop: 56 }, 0, 0.5), { x: 0, y: 344 });
});

test("wheel scroll in frame widths and heights becomes CSS pixels", () => {
  assert.deepEqual(wheelToViewport(desktop, 0.1, -0.5), { dx: 128, dy: -400 });
  assert.deepEqual(wheelToViewport(zoomedPhone, 0, 0.25), { dx: 0, dy: 533.6 });
});
