// Drives solve() against a fake Overpass backend and a real headless
// Chromium, playing the Solver's side of the Bridge socket by hand.

import assert from "node:assert/strict";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { after, before, test } from "node:test";
import { chromium, type Browser, type Page } from "playwright";

import { solve, type SolveOptions } from "../src/index.ts";
import { type Bridge, fakeOverpass } from "./fake-overpass.ts";

const VIEWPORT = { width: 640, height: 480 };

// The iframe is served from localhost and its parent from 127.0.0.1, so the
// two are cross-origin, as reCAPTCHA's iframes are on an Agent's page.
const IFRAME_BOX = { left: 300, top: 200, width: 200, height: 100 };
const PORT_PLACEHOLDER = "{{port}}";
const fixtures: Record<string, string> = {
  "/recaptcha": `<script>window.grecaptcha = { getResponse: () => window.token ?? "" };</script>`,
  "/parent": `<body style="margin:0">
    <iframe src="http://localhost:${PORT_PLACEHOLDER}/child" style="position:absolute;border:0;left:${IFRAME_BOX.left}px;top:${IFRAME_BOX.top}px;width:${IFRAME_BOX.width}px;height:${IFRAME_BOX.height}px"></iframe>
    <script>addEventListener("message", (e) => { if (e.data === "cleared") document.body.dataset.cleared = "true"; });</script>`,
  "/child": `<body style="margin:0"><button style="width:100vw;height:100vh">Verify</button>
    <script>document.querySelector("button").addEventListener("click", (e) => { if (e.isTrusted) parent.postMessage("cleared", "*"); });</script>`,
  // Cleared once trusted typing has put "Eli" into the box and Enter is pressed.
  "/form": `<body style="margin:0"><input id="q" style="position:absolute;left:100px;top:100px;width:200px;height:40px">
    <script>q.addEventListener("keydown", (e) => {
      if (e.isTrusted && e.key === "Enter" && q.value === "Eli") document.body.dataset.cleared = "true";
    });</script>`,
  // Counts trusted clicks and button presses and releases.
  "/clicks": `<body style="margin:0;height:100vh">
    <script>Object.assign(window, { clicks: 0, downs: 0, ups: 0 });
      addEventListener("click", (e) => { if (e.isTrusted) clicks++; });
      addEventListener("mousedown", (e) => { if (e.isTrusted) downs++; });
      addEventListener("mouseup", (e) => { if (e.isTrusted) ups++; });</script>`,
  // burst() repaints ten times, 20ms apart, as reCAPTCHA's fades do, and
  // resolves with the time of the last repaint.
  "/burst": `<div id="box" style="width:200px;height:200px;background:#000"></div>
    <script>window.burst = () => new Promise((done) => {
      let i = 0;
      const t = setInterval(() => {
        box.style.background = "hsl(" + i * 36 + ",80%,50%)";
        if (++i === 10) { clearInterval(t); done(Date.now()); }
      }, 20);
    });</script>`,
};

let browser: Browser;
let site: Server;
let port: number;

before(async () => {
  browser = await chromium.launch();
  site = createServer((req, res) => {
    const html = fixtures[req.url ?? ""];
    if (!html) return void res.writeHead(404).end();
    res.writeHead(200, { "Content-Type": "text/html" }).end(html.replaceAll(PORT_PLACEHOLDER, String(port)));
  });
  await new Promise<void>((resolve) => site.listen(0, resolve)); // all interfaces, so localhost too
  port = (site.address() as AddressInfo).port;
});

after(async () => {
  await browser.close();
  await new Promise((resolve) => site.close(resolve));
});

test("solve with no cleared check waits for reCAPTCHA to issue a response", async () => {
  await claimedSession("/recaptcha", {}, async (page, bridge) => {
    await assert.rejects(bridge.next("solved", 1_500), /timed out/, "reported solved with no reCAPTCHA response");
    await page.evaluate(() => ((window as { token?: string }).token = "any-response-token"));
  });
});

test("a Solver's click reaches a cross-origin iframe at the point clicked", async () => {
  const cleared = (p: Page) => p.evaluate(() => document.body.dataset.cleared === "true");
  await claimedSession("/parent", { cleared }, async (_, bridge) => {
    // Normalized to the frame, as the Queue page sends it.
    const x = (IFRAME_BOX.left + IFRAME_BOX.width / 2) / VIEWPORT.width;
    const y = (IFRAME_BOX.top + IFRAME_BOX.height / 2) / VIEWPORT.height;
    bridge.send({ type: "pointer", action: "down", x, y, t: 0 });
    bridge.send({ type: "pointer", action: "up", x, y, t: 80 });
  });
});

test("a Solver's typing and keys reach the field they clicked into", async () => {
  const cleared = (p: Page) => p.evaluate(() => document.body.dataset.cleared === "true");
  await claimedSession("/form", { cleared }, async (_, bridge) => {
    const x = 200 / VIEWPORT.width;
    const y = 120 / VIEWPORT.height;
    bridge.send({ type: "pointer", action: "down", x, y, t: 0 });
    bridge.send({ type: "pointer", action: "up", x, y, t: 80 });
    bridge.send({ type: "text", text: "Elix", t: 100 });
    bridge.send({ type: "key", key: "Backspace", t: 120 });
    bridge.send({ type: "key", key: "Enter", t: 140 });
  });
});

// claimedSession opens path, calls solve and claims the Task, then lets
// solver act. It passes once the Bridge reports solved and solve returns.
// afterReport runs between the Bridge's solved report and the backend's
// confirmation.
async function claimedSession(
  path: string,
  options: Pick<SolveOptions, "cleared">,
  solver: (page: Page, bridge: Bridge) => Promise<void>,
  afterReport?: (page: Page, bridge: Bridge) => Promise<void>,
) {
  const page = await open(path);
  const overpass = await fakeOverpass();
  const solving = solve(page, { ...options, apiKey: "key", url: overpass.url });
  solving.catch(() => {}); // awaited below unless the test fails first
  try {
    const bridge = await overpass.bridge;
    bridge.send({ type: "claimed", solve_deadline: new Date(Date.now() + 60_000).toISOString() });
    await solver(page, bridge);
    await bridge.next("solved", 3_000);
    await afterReport?.(page, bridge);
    bridge.send({ type: "solved" });
    await solving;
  } finally {
    await overpass.close();
    await page.close();
  }
}

test("the Solver sees the page's final state after a burst of repaints", async () => {
  type BurstWindow = { burst: () => Promise<number>; done?: boolean };
  const cleared = (p: Page) => p.evaluate(() => (window as unknown as BurstWindow).done === true);
  await claimedSession("/burst", { cleared }, async (page, bridge) => {
    await page.waitForTimeout(300); // the first frame is out of the way
    const lastRepaint = await page.evaluate(() => (window as unknown as BurstWindow).burst());
    await page.waitForTimeout(500); // the page is static from here on
    const metadata = bridge.latest("frame")?.metadata as { timestamp: number } | undefined;
    assert.ok(metadata, "no frame reached the Solver");
    const capturedAt = metadata.timestamp * 1000; // seconds since the epoch
    assert.ok(capturedAt >= lastRepaint, `the Solver's last frame is ${Math.round(lastRepaint - capturedAt)}ms stale`);
    await page.evaluate(() => ((window as unknown as BurstWindow).done = true));
  });
});

type ClickWindow = { clicks: number; downs: number; ups: number };
const counts = (p: Page) => p.evaluate(() => {
  const { clicks, downs, ups } = window as unknown as ClickWindow;
  return { clicks, downs, ups };
});

test("the Agent takes the page back once the check is cleared: later Solver input is ignored", async () => {
  const cleared = async (p: Page) => (await counts(p)).clicks >= 1;
  const click = (bridge: Bridge) => {
    bridge.send({ type: "pointer", action: "down", x: 0.5, y: 0.5, t: 0 });
    bridge.send({ type: "pointer", action: "up", x: 0.5, y: 0.5, t: 80 });
  };
  await claimedSession(
    "/clicks",
    { cleared },
    async (_, bridge) => click(bridge),
    async (page, bridge) => {
      click(bridge); // the Solver is still tapping when the check clears
      await page.waitForTimeout(300);
      assert.equal((await counts(page)).clicks, 1);
    },
  );
});

test("a button the Solver still holds is released when the Agent takes over", async () => {
  const cleared = async (p: Page) => (await counts(p)).downs >= 1;
  let after: ClickWindow | undefined;
  const page = await open("/clicks");
  const overpass = await fakeOverpass();
  try {
    const solving = solve(page, { cleared, apiKey: "key", url: overpass.url });
    const bridge = await overpass.bridge;
    bridge.send({ type: "claimed", solve_deadline: new Date(Date.now() + 60_000).toISOString() });
    bridge.send({ type: "pointer", action: "down", x: 0.5, y: 0.5, t: 0 }); // a drag, cut short
    await bridge.next("solved", 3_000);
    bridge.send({ type: "solved" });
    await solving;
    after = await counts(page);
  } finally {
    await overpass.close();
    await page.close();
  }
  assert.deepEqual(after, { clicks: 1, downs: 1, ups: 1 });
});

test("solve creates the Task with the Agent's description of the obstacle", async () => {
  const page = await open("/clicks");
  const overpass = await fakeOverpass();
  try {
    const solving = solve(page, {
      obstacle: "Pass the check on this page.",
      cleared: async () => true,
      apiKey: "key",
      url: overpass.url,
    });
    assert.equal((await overpass.task).obstacle, "Pass the check on this page.");
    const bridge = await overpass.bridge;
    bridge.send({ type: "claimed", solve_deadline: new Date(Date.now() + 60_000).toISOString() });
    await bridge.next("solved", 3_000);
    bridge.send({ type: "solved" });
    await solving;
  } finally {
    await overpass.close();
    await page.close();
  }
});

test("a Solver's done is checked by verify: not cleared leaves the page with the Solver", async () => {
  const page = await open("/clicks");
  const overpass = await fakeOverpass();
  let checks = 0;
  const verify = async (p: Page) => {
    checks++;
    return (await counts(p)).clicks >= 1;
  };
  try {
    const solving = solve(page, { cleared: async () => false, verify, apiKey: "key", url: overpass.url });
    const bridge = await overpass.bridge;
    bridge.send({ type: "claimed", solve_deadline: new Date(Date.now() + 60_000).toISOString() });

    bridge.send({ type: "done" }); // too early: nothing clicked yet
    await bridge.next("not_cleared", 3_000);
    assert.equal(bridge.count("solved"), 0);

    bridge.send({ type: "pointer", action: "down", x: 0.5, y: 0.5, t: 0 }); // the Solver still has the page
    bridge.send({ type: "pointer", action: "up", x: 0.5, y: 0.5, t: 80 });
    bridge.send({ type: "done" });
    await bridge.next("solved", 3_000);
    assert.equal(checks, 2);
    bridge.send({ type: "solved" });
    await solving;
  } finally {
    await overpass.close();
    await page.close();
  }
});

async function open(path: string): Promise<Page> {
  const page = await browser.newPage({ viewport: VIEWPORT });
  await page.goto(`http://127.0.0.1:${port}${path}`); // waits for load, iframes included
  return page;
}
