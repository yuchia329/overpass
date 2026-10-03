// Demo Agent: opens a local fake Challenge page, gets stuck on it and calls
// the Bridge. A Solver on the Queue page clears it by clicking the button.
//
//   UNSTUCK_API_KEY=... npm run demo
//
// Set UNSTUCK_URL if the backend is not on http://localhost:8080.

import { readFile } from "node:fs/promises";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { chromium } from "playwright";

import { solve } from "../src/index.ts";

const challenge = await readFile(new URL("./challenge.html", import.meta.url));
const server = createServer((_, res) => res.writeHead(200, { "Content-Type": "text/html" }).end(challenge));
await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
const { port } = server.address() as AddressInfo;

const browser = await chromium.launch({ headless: process.env.HEADLESS === "1" });
try {
  const page = await browser.newPage({ viewport: { width: 1280, height: 800 } });
  await page.goto(`http://127.0.0.1:${port}/challenge`);
  console.log("Agent: blocked by a Challenge; asking Unstuck for a human.");

  await solve(page, {
    cleared: (p) => p.evaluate(() => document.body.dataset.cleared === "true"),
  });

  console.log("Agent: Challenge cleared; continuing.");
  await page.waitForTimeout(2000);
} finally {
  await browser.close();
  server.close();
}
