// Helpers shared by the LLM-driven demo Agents.

import type { Page } from "playwright";

import type { ClearedCheck } from "../src/index.ts";

const UNSTUCK_URL = (process.env.UNSTUCK_URL ?? "http://localhost:8080").replace(/\/$/, "");

// Reports whether the page holds a CAPTCHA token: from the widget's API, or
// from the hidden response field reCAPTCHA and hCaptcha fill in on success.
// The script is a string: tsx would wrap a function's inner functions in a
// __name helper that does not exist in the page.
const CAPTCHA_TOKEN = `(() => {
  const fromApi = (w) => {
    try {
      return typeof w?.getResponse === "function" && w.getResponse() !== "";
    } catch {
      return false; // hCaptcha throws before it renders
    }
  };
  const fields = document.querySelectorAll('textarea[name="g-recaptcha-response"], textarea[name="h-captcha-response"]');
  return fromApi(window.grecaptcha) || fromApi(window.hcaptcha) || [...fields].some((f) => f.value !== "");
})()`;
export const hasCaptchaToken = (page: Page) => page.evaluate<boolean>(CAPTCHA_TOKEN);

// Cleared once the page holds a CAPTCHA token, or once the blocked page has
// navigated away (e.g. an interstitial that redirects after the check).
export function captchaCleared(blockedUrl: string): ClearedCheck {
  return async (page) => page.url() !== blockedUrl || hasCaptchaToken(page);
}

export async function balance(): Promise<string> {
  const res = await fetch(`${UNSTUCK_URL}/v1/balance`, {
    headers: { Authorization: `Bearer ${process.env.UNSTUCK_API_KEY}` },
  });
  if (!res.ok) return "unknown";
  const { available } = (await res.json()) as { available: number };
  return `${(available / 1e6).toFixed(2)} USDC`;
}

export const say = (who: string, text: string) => console.log(`\n${who}: ${text}`);
