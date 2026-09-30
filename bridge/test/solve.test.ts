// Drives solve() against a fake Overpass backend and a real headless
// Chromium, playing the Solver's side of the Bridge socket by hand.

import assert from "node:assert/strict";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { after, before, test } from "node:test";
import { chromium, type Browser, type Page } from "playwright";
import { WebSocketServer, type WebSocket } from "ws";

import { solve, type SolveOptions } from "../src/index.ts";

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

// claimedSession opens path, calls solve and claims the Task, then lets
// solver act. It passes once the Bridge reports solved and solve returns.
async function claimedSession(
  path: string,
  options: Pick<SolveOptions, "cleared">,
  solver: (page: Page, bridge: Bridge) => Promise<void>,
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

async function open(path: string): Promise<Page> {
  const page = await browser.newPage({ viewport: VIEWPORT });
  await page.goto(`http://127.0.0.1:${port}${path}`); // waits for load, iframes included
  return page;
}

type Bridge = {
  send(msg: object): void;
  /** Resolves with the next message of type, or rejects after ms. */
  next(type: string, ms: number): Promise<Record<string, unknown>>;
  /** The newest message of type received so far and not taken by next. */
  latest(type: string): Record<string, unknown> | undefined;
};

// fakeOverpass accepts one Task and hands the test its Bridge socket.
async function fakeOverpass() {
  const server = createServer((req, res) => {
    if (req.method === "POST" && req.url === "/v1/tasks") {
      res.writeHead(201, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ task_id: "task-1", session_token: "token-1" }));
      return;
    }
    res.writeHead(404).end();
  });
  const wss = new WebSocketServer({ server });
  const bridge = new Promise<Bridge>((resolve) => {
    wss.on("connection", (ws: WebSocket) => {
      // Messages are kept until asked for, so none is lost to a race.
      const received: Record<string, unknown>[] = [];
      const waiting: { type: string; resolve: (m: Record<string, unknown>) => void }[] = [];
      ws.on("message", (data) => {
        const m = JSON.parse(String(data)) as Record<string, unknown>;
        const i = waiting.findIndex((w) => w.type === m.type);
        if (i >= 0) waiting.splice(i, 1)[0].resolve(m);
        else received.push(m);
      });
      resolve({
        send: (msg) => ws.send(JSON.stringify(msg)),
        latest: (type) => received.findLast((m) => m.type === type),
        next: (type, ms) => {
          const i = received.findIndex((m) => m.type === type);
          if (i >= 0) return Promise.resolve(received.splice(i, 1)[0]);
          return new Promise((resolve, reject) => {
            const timer = setTimeout(() => {
              waiting.splice(waiting.indexOf(w), 1);
              reject(new Error(`timed out waiting for ${type}`));
            }, ms);
            const w = {
              type,
              resolve: (m: Record<string, unknown>) => {
                clearTimeout(timer);
                resolve(m);
              },
            };
            waiting.push(w);
          });
        },
      });
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${port}`,
    bridge,
    close: async () => {
      for (const ws of wss.clients) ws.terminate();
      wss.close();
      await new Promise((resolve) => server.close(resolve));
    },
  };
}
