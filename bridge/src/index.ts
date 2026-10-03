// Unstuck Bridge: hands a blocked Playwright page to a human Solver and
// returns once the page's Challenge is cleared.
//
// The Bridge only starts when the Agent calls solve(). It streams the page
// with a CDP screencast and applies the Solver's input through Playwright's
// mouse API, so every event is trusted; it never dispatches DOM events.
// Frames and input go through the backend unless the Solver connects
// directly over WebRTC; the backend socket stays open either way and carries
// the Task's lifecycle.

import type { CDPSession, Frame, Page } from "playwright";

import type { Input } from "./input.ts";
import { type IceServer, Peer, type ScreencastFrame } from "./peer.ts";
import { type FrameMetadata, toViewport, wheelToViewport } from "./viewport.ts";

/** Reports whether the Agent's page is unblocked. */
export type ClearedCheck = (page: Page) => Promise<boolean>;

export interface SolveOptions {
  /** The Customer's API key. Defaults to $UNSTUCK_API_KEY. */
  apiKey?: string;
  /** The Unstuck backend. Defaults to $UNSTUCK_URL, then http://localhost:8080. */
  url?: string;
  /** Reports when the Challenge is cleared. Defaults to a reCAPTCHA check. */
  cleared?: ClearedCheck;
  /**
   * What the Solver is to clear, in one sentence, e.g. "Pass the reCAPTCHA
   * check below the search form." Solvers see it before they claim the
   * Task, so it sets the scope of their work. At most 200 characters.
   */
  obstacle?: string;
  /**
   * Checks the page when the Solver taps Done: the Agent has the page back
   * while it runs. True Solves the Task; false hands the page back to the
   * Solver, who is told the obstacle is still there. Defaults to cleared.
   */
  verify?: ClearedCheck;
  /**
   * Lets the Solver connect directly over WebRTC, taking frames and input off
   * the backend. The Solver and the Agent then see each other's IP address.
   * Needs the optional werift dependency. Defaults to true.
   */
  p2p?: boolean;
}

export class UnstuckError extends Error {
  override name = "UnstuckError";
}

/** Task creation was refused: available Balance is below the Price. */
export class InsufficientBalanceError extends UnstuckError {
  override name = "InsufficientBalanceError";
  constructor(
    readonly available: number,
    readonly price: number,
    readonly serviceWallet: string,
  ) {
    super(
      `Unstuck: insufficient balance: ${usdc(available)} USDC available, the Price of a Task is ${usdc(price)} USDC. ` +
        `Deposit USDC on Solana mainnet from your registered wallet to ${serviceWallet}.`,
    );
  }
}

/** No Solver claimed the Task within the claim window. */
export class TaskExpiredError extends UnstuckError {
  override name = "TaskExpiredError";
  constructor(readonly taskId: string) {
    super(`Unstuck: Task ${taskId} Expired: no Solver claimed it in time.`);
  }
}

/** The Task was claimed but not Solved. */
export class TaskFailedError extends UnstuckError {
  override name = "TaskFailedError";
  constructor(
    readonly taskId: string,
    readonly reason: string,
  ) {
    super(`Unstuck: Task ${taskId} Failed (${reason}).`);
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

type Notice =
  | Input
  | { type: "claimed"; solve_deadline: string; peer_token?: string; ice_servers?: IceServer[] }
  | { type: "rtc_offer"; sdp: string }
  | { type: "done" }
  | { type: "solved" }
  | { type: "expired" }
  | { type: "failed"; reason?: string };

/**
 * Hands page to a human Solver and resolves once the cleared check passes and
 * the Task is Solved. Throws InsufficientBalanceError, TaskExpiredError or
 * TaskFailedError otherwise.
 *
 * The Solver sees exactly the page's viewport. A Solver with a mouse can
 * scroll it but one on a phone cannot, so the whole Challenge should fit in it. Solvers are often on phones, where a small,
 * portrait viewport (e.g. 480x720 for reCAPTCHA's 400x580 image grid) keeps
 * click targets large.
 */
export async function solve(page: Page, options: SolveOptions = {}): Promise<void> {
  const base = (options.url ?? process.env.UNSTUCK_URL ?? "http://localhost:8080").replace(/\/$/, "");
  const apiKey = options.apiKey ?? process.env.UNSTUCK_API_KEY;
  if (!apiKey) throw new UnstuckError("Unstuck: no API key; pass apiKey or set UNSTUCK_API_KEY.");
  const cleared = options.cleared ?? recaptchaCleared;
  const verify = options.verify ?? cleared;
  const p2p = options.p2p ?? true;

  const task = await createTask(base, apiKey, page.url(), options.obstacle);
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
  let inputs = Promise.resolve(); // input events apply one after another, in order
  let shown: FrameMetadata | undefined; // the latest frame sent, which the Solver's input refers to
  // Set while the page is the Agent's: once the check is cleared, and while
  // the Agent checks the Solver's Done. The Solver's input does not apply then.
  let takenOver = false;
  let reported = false; // solved was sent or the Task ended: the page is the Agent's for good
  let checking = false;
  const replay = inputReplay(page);
  const report = () => {
    if (reported) return;
    reported = takenOver = true;
    clearInterval(poll);
    send({ type: "solved" });
  };
  // The Solver tapped Done. The Agent takes the page, lets the Solver's last
  // input land, and checks; if the obstacle is still there the page goes
  // back to the Solver.
  const checkDone = async () => {
    if (checking || reported) return;
    checking = takenOver = true;
    let ok = false;
    try {
      await inputs;
      await replay.release();
      ok = await verify(page);
    } catch (err) {
      console.warn("Unstuck: verify:", err);
    }
    checking = false;
    if (reported) return;
    if (ok) return report();
    takenOver = false;
    send({ type: "not_cleared" });
  };
  const apply = (m: Input) => {
    if (takenOver) return;
    // Input queued before a Done still lands before the check; once solved
    // is reported, nothing queued lands.
    inputs = inputs
      .then(() => (reported ? undefined : replay.apply(shown, m)))
      .catch((err) => console.warn("Unstuck: input:", err));
  };
  let claim: { peerToken: string; iceServers: IceServer[] } | undefined;
  let offered: Peer | undefined; // the newest peer, answered but maybe not ready
  let peer: Peer | undefined; // the Solver's direct connection, once it presented the peer token
  const onNavigated = (frame: Frame) => {
    if (frame !== page.mainFrame()) return;
    send({ type: "url", url: frame.url() });
    peer?.sendURL(frame.url());
  };
  // Frames go to the direct peer while it is up, otherwise to the backend.
  const socketOut: FrameOut = {
    busy: () => socket.bufferedAmount >= MAX_BUFFERED_BYTES,
    send: (f) => socket.send(JSON.stringify({ type: "frame", ...f })),
  };
  const frameOut = (): FrameOut | undefined => {
    if (peer) return { busy: () => peer!.busy(MAX_BUFFERED_BYTES), send: (f) => peer!.sendFrame(f) };
    return socket.readyState === WebSocket.OPEN ? socketOut : undefined;
  };
  const screencast = screencaster(cdp, frameOut, (md) => (shown = md));
  // A new offer replaces any earlier peer: the Solver reloaded or retried.
  const answerOffer = (sdp: string) => {
    if (!claim) return;
    offered?.close();
    const p: Peer = new Peer(claim.peerToken, claim.iceServers, {
      onReady: () => {
        if (offered !== p) return p.close();
        peer = p;
        p.sendURL(page.url());
        screencast.resend(); // the page may be still: show the Solver a frame now
      },
      onInput: apply,
      onClose: () => {
        if (peer !== p) return;
        peer = undefined;
        screencast.resend();
      },
    });
    offered = p;
    void p.answer(sdp).then((answer) => answer && send({ type: "rtc_answer", sdp: answer }));
  };

  try {
    await new Promise<void>((resolve, reject) => {
      socket.onopen = () => {
        send({ type: "url", url: page.url() });
        page.on("framenavigated", onNavigated);
        screencast.start().catch(reject);
      };
      socket.onerror = () => {}; // onclose follows and settles
      // Unstuck Fails a claimed Task whose Bridge disconnects.
      socket.onclose = () => reject(new TaskFailedError(task.task_id, "bridge_disconnected"));
      socket.onmessage = (e) => {
        const m = JSON.parse(String(e.data)) as Notice;
        switch (m.type) {
          case "pointer":
          case "wheel":
          case "text":
          case "key":
            apply(m);
            break;
          case "rtc_offer":
            if (p2p) answerOffer(m.sdp);
            break;
          case "done":
            if (claim || poll) void checkDone();
            break;
          case "claimed":
            if (m.peer_token) claim = { peerToken: m.peer_token, iceServers: m.ice_servers ?? [] };
            poll ??= pollCleared(page, cleared, report);
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
    reported = takenOver = true; // the Task is over: nothing queued lands
    // Let an input event in flight finish, then release a button the Solver
    // still holds: a drag cut short must not leave it held for the Agent.
    await inputs;
    await replay.release().catch(() => {});
    // The Session ends with the Task: the direct connection goes too.
    offered?.close();
    peer?.close();
    page.off("framenavigated", onNavigated);
    socket.onclose = null;
    socket.close();
    await cdp.send("Page.stopScreencast").catch(() => {});
    await cdp.detach().catch(() => {});
  }
}

async function createTask(base: string, apiKey: string, pageURL: string, obstacle?: string) {
  const res = await fetch(`${base}/v1/tasks`, {
    method: "POST",
    headers: { Authorization: `Bearer ${apiKey}`, "Content-Type": "application/json" },
    body: JSON.stringify({ page_url: pageURL, obstacle }),
  });
  const body = (await res.json().catch(() => ({}))) as Record<string, unknown>;
  if (res.status === 402) {
    throw new InsufficientBalanceError(Number(body.available), Number(body.price), String(body.service_wallet));
  }
  if (res.status !== 201) {
    throw new UnstuckError(`Unstuck: creating the Task failed: HTTP ${res.status} ${String(body.error ?? "")}`);
  }
  return body as { task_id: string; session_token: string };
}

/** Where frames go: the backend socket or a direct peer. */
type FrameOut = { busy(): boolean; send(frame: ScreencastFrame & { metadata: FrameMetadata }): void };

// Chrome discards repaints while its frames wait for an ack and does not
// resend them once the page is still, so a late ack would leave the Solver
// looking at a stale frame. Every frame is acked at once instead, and only the
// latest is sent, at most every FRAME_INTERVAL_MS and never while the uplink
// is behind. resend sends the last frame again, for when frames switch
// between the backend and a direct peer.
function screencaster(cdp: CDPSession, out: () => FrameOut | undefined, onSent: (md: FrameMetadata) => void) {
  type Shot = { data: string; metadata: FrameMetadata };
  let latest: Shot | undefined; // the newest frame not yet sent
  let sent: Shot | undefined;
  let lastSent = 0;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const flush = () => {
    timer = undefined;
    const to = out();
    if (latest === undefined || to === undefined) return;
    const wait = lastSent + FRAME_INTERVAL_MS - Date.now();
    if (wait > 0 || to.busy()) {
      timer = setTimeout(flush, Math.max(wait, FRAME_INTERVAL_MS / 4));
      return;
    }
    to.send(latest);
    onSent(latest.metadata);
    sent = latest;
    latest = undefined;
    lastSent = Date.now();
  };
  return {
    async start() {
      cdp.on("Page.screencastFrame", ({ data, metadata, sessionId }) => {
        cdp.send("Page.screencastFrameAck", { sessionId }).catch(() => {});
        latest = { data, metadata };
        if (timer === undefined) flush();
      });
      await cdp.send("Page.startScreencast", { format: "jpeg", quality: JPEG_QUALITY });
    },
    resend() {
      latest ??= sent;
      if (timer === undefined) flush();
    },
  };
}

// Playwright's mouse and keyboard go through CDP input dispatch, so events
// are trusted and reach cross-origin iframes such as reCAPTCHA's. Moves are
// applied as they arrive, so a drag follows the Solver's path. An up can be
// lost (the Solver's connection dropped mid-drag), so a down while the button
// is still held releases it first rather than leaving it stuck, and release
// lets go of it when the Session ends. Keys go to whatever has focus, as the
// Solver's last click left it.
function inputReplay(page: Page) {
  let pressed = false;
  const release = async () => {
    if (!pressed) return;
    pressed = false;
    await page.mouse.up();
  };
  const apply = async (shown: FrameMetadata | undefined, m: Input) => {
    if (m.type === "text") return page.keyboard.type(m.text);
    if (m.type === "key") return page.keyboard.press(m.key);
    const md = shown ?? (await viewportMetadata(page));
    const at = toViewport(md, m.x, m.y);
    await page.mouse.move(at.x, at.y);
    if (m.type === "wheel") {
      const d = wheelToViewport(md, m.dx, m.dy);
      await page.mouse.wheel(d.dx, d.dy);
      return;
    }
    if (m.action === "down") {
      await release();
      await page.mouse.down();
      pressed = true;
    } else if (m.action === "up") {
      await release();
    }
  };
  return { apply, release };
}

// Stands in for frame metadata until the first frame is sent.
async function viewportMetadata(page: Page): Promise<FrameMetadata> {
  const size = page.viewportSize() ?? (await page.evaluate(() => ({ width: innerWidth, height: innerHeight })));
  return { deviceWidth: size.width, deviceHeight: size.height, pageScaleFactor: 1, offsetTop: 0 };
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
