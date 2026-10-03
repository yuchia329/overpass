// Hands one tab of a running Chrome to a human Solver. The Jev demo
// (demo/jev/run.py) runs this when Jev hires a human: Jev is Python and
// drives Chrome over its own CDP connection, so this connects to the same
// Chrome and finds Jev's tab by its CDP target ID. Exits 0 once the obstacle
// is cleared, 1 if Unstuck could not get it cleared.
//
//   tsx demo/jev-handoff.ts <Chrome CDP URL> <target ID> [obstacle]
//
// The Task is Solved when a CAPTCHA issues a token it had not issued before,
// or when the Solver taps Done and Jev agrees the obstacle is gone. To ask
// Jev, this writes VERIFY_REQUEST on a line of its own and reads "yes" or
// "no" back on stdin.

import { createInterface } from "node:readline";

import { type Browser, chromium, type Page } from "playwright";

import { UnstuckError, solve } from "../src/index.ts";
import { balance, hasCaptchaToken, say } from "./common.ts";

const VERIFY_REQUEST = "@@unstuck verify";

const [cdpUrl, targetId, obstacle] = process.argv.slice(2);
if (!cdpUrl || !targetId) {
  console.error("usage: tsx demo/jev-handoff.ts <Chrome CDP URL> <target ID> [obstacle]");
  process.exit(2);
}

async function findTab(browser: Browser, id: string): Promise<Page> {
  for (const page of browser.contexts().flatMap((c) => c.pages())) {
    const cdp = await page.context().newCDPSession(page);
    const { targetInfo } = await cdp.send("Target.getTargetInfo");
    await cdp.detach();
    if (targetInfo.targetId === id) return page;
  }
  throw new Error(`Unstuck: no tab with target ID ${id}`);
}

const replies = createInterface({ input: process.stdin })[Symbol.asyncIterator]();
async function askJev(): Promise<boolean> {
  console.log(VERIFY_REQUEST);
  const { value } = await replies.next();
  return value === "yes";
}

const browser = await chromium.connectOverCDP(cdpUrl);
try {
  const page = await findTab(browser, targetId);
  // A token already on the page is not the Solver's work.
  const hadToken = await hasCaptchaToken(page).catch(() => false);
  const tokenIssued = async (p: Page) => !hadToken && (await hasCaptchaToken(p));
  say("Unstuck", `Balance ${await balance()}.`);
  await solve(page, {
    obstacle,
    cleared: tokenIssued,
    verify: async (p) => (await tokenIssued(p)) || askJev(),
  });
  say("Unstuck", `The Task is Solved. Balance ${await balance()}.`);
} catch (err) {
  if (!(err instanceof UnstuckError)) throw err;
  console.log(`\n${err.message}`); // already starts with "Unstuck:"
  process.exitCode = 1;
} finally {
  // Only disconnects: Jev's tab stays open for Jev.
  await browser.close();
}
// Jev waits for this process to exit before it takes over again, so a handle
// left open (e.g. by the WebRTC stack or stdin) must not keep it alive.
process.exit();
