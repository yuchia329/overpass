// Overpass Bridge: hands a blocked Playwright page to a human Solver and
// returns once the page's Challenge is cleared.
//
// The Bridge only starts when the Agent calls solve(). It streams the page
// with a CDP screencast and applies the Solver's input through Playwright's
// mouse API, so every event is trusted; it never dispatches DOM events.

import type { CDPSession, Frame, Page } from "playwright";

/** Reports whether the Agent's page is unblocked. */
export type ClearedCheck = (page: Page) => Promise<boolean>;

export interface SolveOptions {
  /** The Customer's API key. Defaults to $OVERPASS_API_KEY. */
  apiKey?: string;
  /** The Overpass backend. Defaults to $OVERPASS_URL, then http://localhost:8080. */
  url?: string;
  /** Reports when the Challenge is cleared. Defaults to a reCAPTCHA check. */
  cleared?: ClearedCheck;
}

export class OverpassError extends Error {
  override name = "OverpassError";
}

/** Task creation was refused: available Balance is below the Price. */
export class InsufficientBalanceError extends OverpassError {
  override name = "InsufficientBalanceError";
  constructor(
    readonly available: number,
    readonly price: number,
    readonly serviceWallet: string,
  ) {
    super(
      `Overpass: insufficient balance: ${usdc(available)} USDC available, the Price of a Task is ${usdc(price)} USDC. ` +
        `Deposit USDC on Solana mainnet from your registered wallet to ${serviceWallet}.`,
    );
  }
}

/** No Solver claimed the Task within the claim window. */
export class TaskExpiredError extends OverpassError {
  override name = "TaskExpiredError";
  constructor(readonly taskId: string) {
    super(`Overpass: Task ${taskId} Expired: no Solver claimed it in time.`);
  }
}

/** The Task was claimed but not Solved. */
export class TaskFailedError extends OverpassError {
  override name = "TaskFailedError";
  constructor(
    readonly taskId: string,
    readonly reason: string,
  ) {
    super(`Overpass: Task ${taskId} Failed (${reason}).`);
  }
}

/** The default cleared check: reCAPTCHA has issued a response token. */
export const recaptchaCleared: ClearedCheck = (page) =>
  page.evaluate(() => {
    const g = (window as { grecaptcha?: { getResponse?: () => string } }).grecaptcha;
    return typeof g?.getResponse === "function" && g.getResponse() !== "";
  });

const CLEARED_POLL_MS = 500;
const FRAME_INTERVAL_MS = 100; // about 10 fps
const JPEG_QUALITY = 60;
const MAX_BUFFERED_BYTES = 1 << 20; // skip frames while the uplink is this far behind

type Pointer = { type: "pointer"; action: "down" | "move" | "up"; x: number; y: number; t: number };
type Notice =
  | Pointer
  | { type: "claimed"; solve_deadline: string }
  | { type: "solved" }
  | { type: "expired" }
  | { type: "failed"; reason?: string };

/**
 * Hands page to a human Solver and resolves once the cleared check passes and
 * the Task is Solved. Throws InsufficientBalanceError, TaskExpiredError or
 * TaskFailedError otherwise.
 */
export async function solve(page: Page, options: SolveOptions = {}): Promise<void> {
  const base = (options.url ?? process.env.OVERPASS_URL ?? "http://localhost:8080").replace(/\/$/, "");
  const apiKey = options.apiKey ?? process.env.OVERPASS_API_KEY;
  if (!apiKey) throw new OverpassError("Overpass: no API key; pass apiKey or set OVERPASS_API_KEY.");
  const cleared = options.cleared ?? recaptchaCleared;

  const task = await createTask(base, apiKey, page.url());
  const cdp = await page.context().newCDPSession(page);
  // No await between opening the socket and setting its handlers below, so
  // no event is missed.
  const socket = new WebSocket(
    `${base.replace(/^http/, "ws")}/v1/tasks/${task.task_id}/bridge?token=${encodeURIComponent(task.session_token)}`,
  );
  const send = (msg: object) => {
    if (socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify(msg));
  };

  let poll: ReturnType<typeof setInterval> | undefined;
  let inputs = Promise.resolve(); // pointer events apply one after another, in order
  const onNavigated = (frame: Frame) => {
    if (frame === page.mainFrame()) send({ type: "url", url: frame.url() });
  };

  try {
    await new Promise<void>((resolve, reject) => {
      socket.onopen = () => {
        send({ type: "url", url: page.url() });
        page.on("framenavigated", onNavigated);
        startScreencast(cdp, socket).catch(reject);
      };
      socket.onerror = () => {}; // onclose follows and settles
      // Overpass Fails a claimed Task whose Bridge disconnects.
      socket.onclose = () => reject(new TaskFailedError(task.task_id, "bridge_disconnected"));
      socket.onmessage = (e) => {
        const m = JSON.parse(String(e.data)) as Notice;
        switch (m.type) {
          case "pointer":
            inputs = inputs.then(() => applyPointer(page, m)).catch((err) => console.warn("Overpass: input:", err));
            break;
          case "claimed":
            poll ??= pollCleared(page, cleared, () => {
              clearInterval(poll);
              send({ type: "solved" });
            });
            break;
          case "solved":
            resolve();
            break;
          case "expired":
            reject(new TaskExpiredError(task.task_id));
            break;
          case "failed":
            reject(new TaskFailedError(task.task_id, m.reason ?? "unknown"));
            break;
        }
      };
    });
  } finally {
    clearInterval(poll);
    page.off("framenavigated", onNavigated);
    socket.onclose = null;
    socket.close();
    await cdp.send("Page.stopScreencast").catch(() => {});
    await cdp.detach().catch(() => {});
  }
}

async function createTask(base: string, apiKey: string, pageURL: string) {
  const res = await fetch(`${base}/v1/tasks`, {
    method: "POST",
    headers: { Authorization: `Bearer ${apiKey}`, "Content-Type": "application/json" },
    body: JSON.stringify({ page_url: pageURL }),
  });
  const body = (await res.json().catch(() => ({}))) as Record<string, unknown>;
  if (res.status === 402) {
    throw new InsufficientBalanceError(Number(body.available), Number(body.price), String(body.service_wallet));
  }
  if (res.status !== 201) {
    throw new OverpassError(`Overpass: creating the Task failed: HTTP ${res.status} ${String(body.error ?? "")}`);
  }
  return body as { task_id: string; session_token: string };
}

// Chrome sends the next frame only after the last is acked, so acking after
// FRAME_INTERVAL_MS caps the frame rate without ever dropping the latest one.
async function startScreencast(cdp: CDPSession, socket: WebSocket) {
  cdp.on("Page.screencastFrame", ({ data, metadata, sessionId }) => {
    if (socket.readyState === WebSocket.OPEN && socket.bufferedAmount < MAX_BUFFERED_BYTES) {
      socket.send(JSON.stringify({ type: "frame", data, metadata }));
    }
    setTimeout(() => cdp.send("Page.screencastFrameAck", { sessionId }).catch(() => {}), FRAME_INTERVAL_MS);
  });
  await cdp.send("Page.startScreencast", { format: "jpeg", quality: JPEG_QUALITY });
}

// Playwright's mouse goes through CDP input dispatch, so events are trusted
// and reach cross-origin iframes such as reCAPTCHA's.
async function applyPointer(page: Page, p: Pointer) {
  const size =
    page.viewportSize() ?? (await page.evaluate(() => ({ width: innerWidth, height: innerHeight })));
  await page.mouse.move(p.x * size.width, p.y * size.height);
  if (p.action === "down") await page.mouse.down();
  if (p.action === "up") await page.mouse.up();
}

function pollCleared(page: Page, cleared: ClearedCheck, onCleared: () => void) {
  let checking = false;
  return setInterval(async () => {
    if (checking) return;
    checking = true;
    try {
      if (await cleared(page)) onCleared();
    } catch {
      // The page may be navigating; check again next time.
    } finally {
      checking = false;
    }
  }, CLEARED_POLL_MS);
}

function usdc(units: number) {
  return (units / 1e6).toString();
}
