// Demo Agent against a real reCAPTCHA: opens Google's reCAPTCHA demo page,
// gets stuck on the checkbox and calls the Bridge with no cleared check, so
// the reCAPTCHA default applies. A Solver on the Queue page ticks the
// checkbox and clears any image grid by clicking; the Agent then
// submits the form.
//
//   UNSTUCK_API_KEY=... npm run demo:recaptcha
//
// Set UNSTUCK_URL if the backend is not on http://localhost:8080.

import { chromium } from "playwright";

import { solve } from "../src/index.ts";

const DEMO_URL = "https://www.google.com/recaptcha/api2/demo";

const browser = await chromium.launch({ headless: process.env.HEADLESS === "1" });
try {
  // A phone-shaped viewport: the image grid (400x580) fits without
  // scrolling and its tiles stay large enough to tap on the Solver's phone.
  const page = await browser.newPage({ viewport: { width: 480, height: 720 } });
  await page.goto(DEMO_URL);
  await page.frameLocator('iframe[title="reCAPTCHA"]').locator("#recaptcha-anchor").waitFor();
  console.log("Agent: blocked by reCAPTCHA; asking Unstuck for a human.");

  await solve(page);

  console.log("Agent: reCAPTCHA cleared; submitting the form.");
  await page.locator("#recaptcha-demo-submit").click();
  const result = page.locator(".recaptcha-success, .recaptcha-error-message").first();
  console.log(`Agent: Google says: ${(await result.textContent())?.trim()}`);
  await page.waitForTimeout(2000);
} finally {
  await browser.close();
}
