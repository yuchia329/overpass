// Drives solve() against a fake Unstuck backend and a real headless
// Chromium, with the Solver connected directly over WebRTC.

import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { chromium, type Browser, type Page } from "playwright";

import { solve, TaskFailedError, type SolveOptions } from "../src/index.ts";
import { type Bridge, fakeUnstuck } from "./fake-unstuck.ts";
import { connectPeer, type SolverPeer } from "./solver-peer.ts";

const TOKEN = "pt_session";

// A full-page button that records a trusted click, repainting every 30ms so
// frames keep flowing.
const PAGE = `<body style="margin:0"><button id="b" style="width:100vw;height:100vh">Verify</button>
<script>
  b.addEventListener("click", (e) => { if (e.isTrusted) window.cleared = true; });
  let i = 0;
  setInterval(() => { b.style.background = "hsl(" + (i++ * 10) % 360 + ",80%,50%)"; }, 30);
</script>`;

type ClearedWindow = { cleared?: boolean };
const cleared = (p: Page) => p.evaluate(() => (window as ClearedWindow).cleared === true);

let browser: Browser;
before(async () => {
  browser = await chromium.launch();
});
after(async () => {
  await browser.close();
});

test("a peer with the Session's token gets the frames and drives the page directly", async () => {
  await session({}, async ({ bridge, solving, connect }) => {
    const peer = await connect(TOKEN);
    await peer.next("welcome", 5_000);
    await until(() => peer.frames.length > 0, "no frame over the peer connection");
    assert.deepEqual([...peer.frames[0].bytes.subarray(0, 2)], [0xff, 0xd8], "the frame is not a JPEG");

    await delay(300); // a frame already on the socket may still land
    const relayed = bridge.count("frame");
    const direct = peer.frames.length;
    await delay(500);
    assert.equal(bridge.count("frame"), relayed, "frames still go through the backend");
    assert.ok(peer.frames.length > direct, "the repainting page sent no more frames to the peer");

    peer.send({ type: "pointer", action: "down", x: 0.5, y: 0.5, t: 0 });
    peer.send({ type: "pointer", action: "up", x: 0.5, y: 0.5, t: 80 });
    await bridge.next("solved", 3_000);
    bridge.send({ type: "solved" });
    await solving;
    await within(peer.closed, "the peer connection outlived the Task");
  });
});

test("a peer without the Session's token is dropped and cannot drive the page", async () => {
  await session({}, async ({ page, bridge, solving, connect }) => {
    const peer = await connect("pt_someone_else");
    peer.send({ type: "pointer", action: "down", x: 0.5, y: 0.5, t: 0 });
    peer.send({ type: "pointer", action: "up", x: 0.5, y: 0.5, t: 80 });

    await within(peer.closed, "the Bridge kept a peer with the wrong token");
    await assert.rejects(peer.next("welcome", 0), /timed out/);
    assert.equal(await cleared(page), false, "the peer's click reached the page");
    bridge.send({ type: "failed", reason: "gave_up" });
    await assert.rejects(solving, TaskFailedError);
  });
});

test("frames go back to the socket when the peer goes away", async () => {
  await session({}, async ({ bridge, solving, connect }) => {
    const peer = await connect(TOKEN);
    await peer.next("welcome", 5_000);
    await delay(300);
    const relayed = bridge.count("frame");

    peer.close();

    await until(() => bridge.count("frame") > relayed, "frames did not return to the socket");
    bridge.send({ type: "pointer", action: "down", x: 0.5, y: 0.5, t: 0 });
    bridge.send({ type: "pointer", action: "up", x: 0.5, y: 0.5, t: 80 });
    await bridge.next("solved", 3_000);
    bridge.send({ type: "solved" });
    await solving;
  });
});

test("with p2p off the Bridge does not answer an offer", async () => {
  await session({ p2p: false }, async ({ bridge, solving, connect }) => {
    await assert.rejects(connect(TOKEN, 1_000), /timed out waiting for rtc_answer/);
    bridge.send({ type: "failed", reason: "gave_up" });
    await assert.rejects(solving, TaskFailedError);
  });
});

type Connect = (token: string, answerMs?: number) => Promise<SolverPeer>;

// session opens PAGE, calls solve and claims the Task with a peer token,
// then lets the test play the Solver.
async function session(
  options: Pick<SolveOptions, "p2p">,
  solver: (s: { page: Page; bridge: Bridge; solving: Promise<void>; connect: Connect }) => Promise<void>,
) {
  const page = await browser.newPage({ viewport: { width: 400, height: 300 } });
  await page.setContent(PAGE);
  const unstuck = await fakeUnstuck();
  const solving = solve(page, { ...options, cleared, apiKey: "key", url: unstuck.url });
  solving.catch(() => {}); // the test awaits it
  const peers: SolverPeer[] = [];
  try {
    const bridge = await unstuck.bridge;
    bridge.send({
      type: "claimed",
      solve_deadline: new Date(Date.now() + 60_000).toISOString(),
      peer_token: TOKEN,
      ice_servers: [],
    });
    const connect: Connect = async (token, answerMs) => {
      const peer = await connectPeer(bridge, token, answerMs);
      peers.push(peer);
      return peer;
    };
    await solver({ page, bridge, solving, connect });
  } finally {
    for (const peer of peers) peer.close();
    await unstuck.close();
    await page.close();
  }
}

async function until(cond: () => boolean, msg: string, ms = 3_000) {
  const deadline = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > deadline) assert.fail(msg);
    await delay(20);
  }
}

async function within(p: Promise<unknown>, msg: string, ms = 3_000) {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise((_, reject) => (timer = setTimeout(() => reject(new Error(msg)), ms)));
  try {
    await Promise.race([p, timeout]);
  } finally {
    clearTimeout(timer);
  }
}
