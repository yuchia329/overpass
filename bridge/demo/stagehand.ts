// Demo Agent driven by an LLM: a Stagehand agent works on a task in a real
// website and tries the reCAPTCHA in its way itself. After three failed
// attempts the demo pauses it and hands the same browser tab to a human
// Solver through the Bridge, then tells the agent to carry on.
//
//   ANTHROPIC_API_KEY=... OVERPASS_API_KEY=... \
//   AGENT_URL=https://example.com AGENT_TASK="..." npm run demo:stagehand
//
// Playwright launches Chromium with a CDP port and Stagehand attaches to it,
// so solve() gets a Playwright page for the very tab the agent drives.
// Set AGENT_MODEL to change the model, OVERPASS_URL for another backend.

import { Stagehand } from "@browserbasehq/stagehand";
import type { LanguageModelMiddleware } from "ai";
import { chromium, type Page } from "playwright";

import { OverpassError, solve } from "../src/index.ts";
import { balance, captchaCleared, hasCaptchaToken, say } from "./common.ts";

const START_URL = process.env.AGENT_URL ?? "https://www.google.com/recaptcha/api2/demo";
const TASK =
  process.env.AGENT_TASK ??
  "Fill in the demo form with any name and submit it, then report the message the page shows.";
const MODEL = process.env.AGENT_MODEL ?? "anthropic/claude-sonnet-4-6";
// "hybrid" works from screenshots; "dom" from the accessibility tree, for
// text-only models.
const MODE = process.env.AGENT_MODE === "dom" ? "dom" : "hybrid";
const CDP_PORT = 9333;

// The agent tries a reCAPTCHA image challenge itself; after MAX_ATTEMPTS
// submits (Verify, Next or Skip) without a token, the demo pauses it and hands
// the tab to a human through Overpass. Code counts the attempts: a model
// asked to count its own failures stops counting.
const MAX_ATTEMPTS = 3;

const SYSTEM_PROMPT = `You are a browser agent completing a task for your user.
Work the way a person would: fill in every form field the task needs first,
and deal with any human check (a CAPTCHA such as "I'm not a robot") only after
that, just before you submit.

When you reach a human check, try to pass it yourself: click the checkbox
and, if an image challenge opens, select the matching images and click
Verify. If a message tells you a human has passed the check, do not touch
the CAPTCHA again: continue the task, for example by submitting the form the
check was guarding.`;

// The order of work, prepended to the user's task: the instruction carries
// more weight with small models than the system prompt does.
const PLAN = `Do this in order:
1. Fill in the search criteria the task needs. Do not touch any CAPTCHA yet.
2. Pass the human check.
3. Submit the search and read the result.`;

const HUMAN_PASSED = `A human has passed the CAPTCHA for you through Overpass. It is done,
even if it does not look ticked. Do not click the CAPTCHA checkbox or challenge
again. Continue with the task now: submit the search and report the result.`;

// Describes the reCAPTCHA widget for the waiting log: whether its checkbox is
// ticked, and whether an image challenge is open and what it says.
async function recaptchaState(page: Page): Promise<string> {
  const anchor = page.frames().find((f) => f.url().includes("/recaptcha/api2/anchor"));
  const challenge = page.frames().find((f) => f.url().includes("/recaptcha/api2/bframe"));
  if (!anchor) return "no reCAPTCHA on the page";
  const checked = await anchor
    .locator("#recaptcha-anchor")
    .getAttribute("aria-checked", { timeout: 1000 })
    .catch(() => "?");
  const prompt = challenge
    ? await challenge
        .locator(".rc-imageselect-instructions, .rc-imageselect-incorrect-response:visible, .rc-imageselect-error-select-more:visible")
        .allInnerTexts()
        .then((t) => t.join(" ").replace(/\s+/g, " ").trim())
        .catch(() => "")
    : "";
  return `checkbox ticked: ${checked}${prompt ? `, challenge: "${prompt.slice(0, 80)}"` : ""}`;
}

// Counts presses of the challenge's submit button (it reads Verify, Next or
// Skip) since the challenge frame loaded, installing the listener on first
// call. Clicks reach it as trusted CDP input, so a capturing listener sees them.
async function challengeSubmits(page: Page): Promise<number | undefined> {
  const frame = page.frames().find((f) => /\/recaptcha\/(api2|enterprise)\/bframe/.test(f.url()));
  return frame?.evaluate(() => {
    const w = window as { __overpassSubmits?: number };
    if (w.__overpassSubmits === undefined) {
      w.__overpassSubmits = 0;
      document.addEventListener(
        "click",
        (e) => {
          if ((e.target as Element | null)?.closest?.("#recaptcha-verify-button")) w.__overpassSubmits!++;
        },
        true,
      );
    }
    return w.__overpassSubmits;
  });
}

// Some models (e.g. MiniMax-M3) answer Stagehand's act schema with
// "arguments": "" where it expects an array, which fails validation. Wrap a
// string in an array before the SDK validates the JSON.
function repairArguments(json: string): string {
  try {
    return JSON.stringify(JSON.parse(json), (key, value) =>
      key === "arguments" && typeof value === "string" ? (value === "" ? [] : [value]) : value,
    );
  } catch {
    return json;
  }
}

// For structured output the Anthropic provider forces a "json" tool and drops
// any text. MiniMax-M3 sometimes replies with the JSON as plain text instead,
// leaving no content at all; recover that text from the raw response.
function rawText(body: unknown): string {
  const blocks = (body as { content?: { type: string; text?: string }[] } | undefined)?.content ?? [];
  return blocks
    .filter((b) => b.type === "text" && b.text)
    .map((b) => b.text!)
    .join("")
    .trim()
    .replace(/^```(?:json)?\s*|\s*```$/g, "");
}

const repairMiddleware: LanguageModelMiddleware = {
  wrapGenerate: async ({ doGenerate, params }) => {
    const result = await doGenerate();
    let content = result.content.map((part) => {
      if (part.type === "text") return { ...part, text: repairArguments(part.text) };
      if (part.type === "tool-call") return { ...part, input: repairArguments(part.input) };
      return part;
    });
    const text = params.responseFormat?.type === "json" && content.length === 0 ? rawText(result.response?.body) : "";
    if (text) content = [{ type: "text", text: repairArguments(text) }];
    return { ...result, content };
  },
};

// AGENT_BASE_URL points the provider at a compatible endpoint, e.g. MiniMax's
// Anthropic API (https://api.minimax.io/anthropic/v1) with
// AGENT_MODEL=anthropic/MiniMax-M3.
const apiKey = process.env.AGENT_API_KEY ?? process.env.ANTHROPIC_API_KEY;
if (!apiKey) throw new Error("Set ANTHROPIC_API_KEY or AGENT_API_KEY.");
const model = {
  modelName: MODEL,
  apiKey,
  middleware: repairMiddleware,
  ...(process.env.AGENT_BASE_URL && { baseURL: process.env.AGENT_BASE_URL }),
};

const browser = await chromium.launch({
  headless: process.env.HEADLESS === "1",
  args: [`--remote-debugging-port=${CDP_PORT}`],
});
const context = await browser.newContext({ viewport: { width: 800, height: 900 } });
const page: Page = await context.newPage();
const { webSocketDebuggerUrl } = (await (await fetch(`http://127.0.0.1:${CDP_PORT}/json/version`)).json()) as {
  webSocketDebuggerUrl: string;
};

const stagehand = new Stagehand({
  env: "LOCAL",
  experimental: true, // agent callbacks are experimental in Stagehand v3
  disableAPI: true,
  verbose: 0,
  model,
  localBrowserLaunchOptions: { cdpUrl: webSocketDebuggerUrl },
});

try {
  await stagehand.init();
  await page.goto(START_URL);
  say("Overpass", `Balance ${await balance()}.`);
  say("User", TASK);

  let steps = 0;
  let attempts = 0; // challenge submits that did not pass the check
  let lastRaw = 0; // the challenge frame's own count, which restarts if it reloads
  let handedOff = false;

  // Hands the tab to a human and waits until the check is passed. Returns the
  // message for the agent.
  async function handToHuman(): Promise<string> {
    say("Agent", `Still blocked after ${attempts} attempts. Hiring a human through Overpass for 0.01 USDC.`);
    const started = Date.now();
    const waiting = setInterval(async () => {
      const token = await hasCaptchaToken(page).catch((err: Error) => `error: ${err.message}`);
      const secs = Math.round((Date.now() - started) / 1000);
      const state = await recaptchaState(page).catch((err: Error) => `state error: ${err.message}`);
      say("Agent", `Waiting for the human (${secs}s, CAPTCHA token: ${token === true ? "yes" : token || "not yet"}; ${state}).`);
    }, 10_000);
    try {
      await solve(page, { cleared: captchaCleared(page.url()) });
    } catch (err) {
      if (!(err instanceof OverpassError)) throw err;
      say("Overpass", err.message);
      return `Human help failed (${err.message}). Stop and report that the CAPTCHA blocked you.`;
    } finally {
      clearInterval(waiting);
    }
    say("Overpass", `A human cleared the check. Balance ${await balance()}. The agent takes over again.`);
    return HUMAN_PASSED;
  }

  const agent = stagehand.agent({
    mode: MODE,
    model,
    systemPrompt: SYSTEM_PROMPT,
  });

  const result = await agent.execute({
    instruction: `${TASK}\n\n${PLAN}`,
    page,
    maxSteps: 60,
    highlightCursor: true,
    callbacks: {
      // Runs before every model call. Counts the agent's failed challenge
      // submits and, once it has used up its attempts, pauses it until a
      // human has passed the check, then tells it so.
      prepareStep: async (options) => {
        if (handedOff) return options;
        const raw = await challengeSubmits(page).catch(() => undefined);
        if (raw === undefined) return options;
        const added = raw < lastRaw ? raw : raw - lastRaw; // a reloaded frame starts again at 0
        lastRaw = raw;
        if (added === 0 || (await hasCaptchaToken(page).catch(() => false))) return options;
        for (let i = 0; i < added && attempts < MAX_ATTEMPTS; i++) {
          attempts++;
          say("Agent", `CAPTCHA attempt ${attempts}/${MAX_ATTEMPTS} failed.`);
        }
        if (attempts < MAX_ATTEMPTS) return options;
        handedOff = true;
        options.messages.push({ role: "user", content: await handToHuman() });
        return options;
      },
      onStepFinish: (step) => {
        steps++;
        if (step.text.trim()) say("Agent", step.text.trim());
        for (const call of step.toolCalls) {
          console.log(`  -> ${call.toolName} ${JSON.stringify(call.input)}`);
        }
      },
    },
  });
  say("Agent", `${result.message} (${steps} steps)`);
  await page.waitForTimeout(3000);
} finally {
  await stagehand.close().catch(() => {});
  await browser.close();
}
