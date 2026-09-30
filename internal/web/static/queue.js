// Queue page: a Solver connects with their wallet, watches Pending Tasks
// arrive live, and Claims one. During the Session the Agent's page is shown
// live and the Solver's pointer and wheel input is sent to the Bridge.
"use strict";

const $ = (id) => document.getElementById(id);
const tasks = new Map(); // task id -> { pageURL, since } where since is local ms when waited_ms was 0
const claiming = new Map(); // task id -> page URL, while a Claim is in flight
const early = new Map(); // task id -> frame that arrived before its claimed reply
let socket = null;
let wallet = "";
let claim = null; // { id, pageURL, deadline } for the Task this Solver holds
let retry = 0;

// The demo Solver's wallet fills in when none is saved, and can be copied
// into a Solana wallet app.
const DEMO_WALLET = $("demo-wallet-address").textContent;
$("wallet").value = DEMO_WALLET;
try { $("wallet").value = localStorage.getItem("overpass.wallet") || DEMO_WALLET; } catch {}

$("copy-demo-wallet").addEventListener("click", async () => {
  const button = $("copy-demo-wallet");
  try {
    await navigator.clipboard.writeText(DEMO_WALLET);
    button.textContent = "Copied";
    setTimeout(() => { button.textContent = "Copy"; }, 1500);
  } catch {
    // No clipboard access (plain http, or denied): select it for a manual copy.
    getSelection().selectAllChildren($("demo-wallet-address"));
  }
});

$("connect").addEventListener("submit", (e) => {
  e.preventDefault();
  wallet = $("wallet").value.trim();
  if (!/^[1-9A-HJ-NP-Za-km-z]{32,44}$/.test(wallet)) {
    notice("That is not a Solana wallet address.");
    return;
  }
  notice("");
  try { localStorage.setItem("overpass.wallet", wallet); } catch {}
  retry = 0;
  connect();
});

$("give-up").addEventListener("click", () => {
  if (claim) send({ type: "give_up", task_id: claim.id });
});

// Pointer Events cover mouse and touch alike. Positions are sent normalized
// to 0–1 of the displayed frame, so they do not depend on its size here; the
// Bridge maps them onto the Agent's page with the frame's metadata. Only the
// primary pointer is relayed: a second finger does nothing.
const screen = $("screen");
const INPUT_INTERVAL_MS = 25; // moves and wheel scrolls go out at up to 40 Hz
let pressed = false; // a down was sent and its up was not
let lastInput = 0;
let pendingMove = null; // the latest move not yet sent
let pendingWheel = null; // { e, dx, dy }: wheel scroll not yet sent, in frame sizes
let inputTimer = 0;

screen.addEventListener("pointerdown", (e) => {
  if (!e.isPrimary || e.button !== 0) return;
  e.preventDefault();
  screen.setPointerCapture(e.pointerId);
  flushInput();
  pressed = pointer("down", e);
});
screen.addEventListener("pointermove", (e) => {
  if (!e.isPrimary) return;
  pendingMove = e;
  scheduleInput();
});
const release = (e) => {
  if (!e.isPrimary || !pressed) return;
  pressed = false;
  flushInput();
  pointer("up", e);
};
screen.addEventListener("pointerup", release);
screen.addEventListener("pointercancel", release);
screen.addEventListener("contextmenu", (e) => e.preventDefault());
screen.addEventListener("wheel", (e) => {
  if (!claim || screen.hidden) return;
  e.preventDefault(); // scroll the Agent's page, not this one
  const r = screen.getBoundingClientRect();
  // deltaMode: 0 pixels, 1 lines, 2 pages
  const unit = [1, 16, r.height][e.deltaMode] || 1;
  const w = pendingWheel || (pendingWheel = { dx: 0, dy: 0 });
  w.e = e;
  w.dx += (e.deltaX * unit) / r.width;
  w.dy += (e.deltaY * unit) / r.height;
  scheduleInput();
}, { passive: false });

// scheduleInput sends pending moves and wheel scrolls at most every
// INPUT_INTERVAL_MS, keeping only the latest move and summing wheel scrolls.
function scheduleInput() {
  if (inputTimer) return;
  inputTimer = setTimeout(flushInput, Math.max(0, lastInput + INPUT_INTERVAL_MS - performance.now()));
}

// flushInput sends pending input now, so it keeps its order with a down or up.
function flushInput() {
  clearTimeout(inputTimer);
  inputTimer = 0;
  lastInput = performance.now();
  if (pendingMove) pointer("move", pendingMove);
  if (pendingWheel) {
    const at = position(pendingWheel.e);
    if (at) send({ type: "wheel", task_id: claim.id, ...at, dx: pendingWheel.dx, dy: pendingWheel.dy, t: pendingWheel.e.timeStamp });
  }
  pendingMove = pendingWheel = null;
}

// pointer sends a pointer event and reports whether it was sent.
function pointer(action, e) {
  const at = position(e);
  if (!at) return false;
  send({ type: "pointer", task_id: claim.id, action, ...at, t: e.timeStamp });
  return true;
}

// position is where e is on the displayed frame, or null outside a Session.
function position(e) {
  if (!claim || screen.hidden) return null;
  const r = screen.getBoundingClientRect();
  const clamp = (v) => Math.min(1, Math.max(0, v));
  return { x: clamp((e.clientX - r.left) / r.width), y: clamp((e.clientY - r.top) / r.height) };
}

function connect() {
  if (socket) socket.close();
  tasks.clear();
  claim = null; // the server resends a Claim this wallet still holds
  pressed = false;
  render();
  const scheme = location.protocol === "https:" ? "wss" : "ws";
  const ws = new WebSocket(`${scheme}://${location.host}/v1/queue?wallet=${encodeURIComponent(wallet)}`);
  socket = ws;
  status("Connecting…");
  ws.onopen = () => { retry = 0; status(`Connected as ${short(wallet)}.`); };
  ws.onmessage = (e) => handle(JSON.parse(e.data));
  ws.onclose = () => {
    if (socket !== ws) return; // replaced by a newer connection
    socket = null;
    const delay = Math.min(1000 * 2 ** retry++, 10000);
    status(`Disconnected. Reconnecting in ${delay / 1000}s…`);
    setTimeout(() => { if (!socket) connect(); }, delay);
  };
}

function handle(m) {
  if (m.type === "frame") {
    showFrame(m); // frames arrive often; they do not rebuild the list
    return;
  }
  switch (m.type) {
    case "task_added":
      tasks.set(m.task_id, { pageURL: m.page_url, since: Date.now() - m.waited_ms });
      break;
    case "task_removed":
      tasks.delete(m.task_id);
      break;
    case "claimed":
      // A Claim just won carries solve_window_ms; one resumed on reconnect
      // carries solve_left_ms and its page URL.
      claim = {
        id: m.task_id,
        pageURL: m.page_url || claiming.get(m.task_id) || "",
        deadline: Date.now() + (m.solve_left_ms ?? m.solve_window_ms),
      };
      claiming.delete(m.task_id);
      if (early.has(m.task_id)) {
        showFrame(early.get(m.task_id));
        early.delete(m.task_id);
      }
      notice("");
      break;
    case "claim_failed":
      claiming.delete(m.task_id);
      early.delete(m.task_id);
      notice({
        already_claimed: "Already claimed by another Solver.",
        expired: "That Task has Expired.",
        holding_claim: "Finish or give up your current Task first.",
      }[m.error] || `Claim failed: ${m.error}.`);
      break;
    case "task_failed":
      if (claim && claim.id === m.task_id) claim = null;
      notice({
        gave_up: "You gave up the Task.",
        bridge_disconnected: "Agent disconnected; the Task Failed.",
      }[m.reason] || "Solve window passed; the Task Failed.");
      break;
    case "task_solved":
      if (claim && claim.id === m.task_id) claim = null;
      notice(`Solved! Earning of ${usdc(m.earning)} USDC recorded.`);
      break;
    case "error":
      notice(`Error: ${m.error}.`);
      break;
  }
  render();
}

function send(msg) {
  if (socket && socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify(msg));
}

function render() {
  const list = $("tasks");
  list.replaceChildren(...[...tasks].map(([id, t]) => {
    const li = document.createElement("li");
    const info = document.createElement("div");
    info.className = "info";
    const url = document.createElement("div");
    url.className = "url";
    url.textContent = t.pageURL;
    const waited = document.createElement("div");
    waited.className = "waited";
    waited.dataset.since = t.since;
    info.append(url, waited);
    const btn = document.createElement("button");
    btn.textContent = "Claim";
    btn.disabled = claiming.has(id) || claim !== null;
    btn.onclick = () => {
      claiming.set(id, t.pageURL);
      send({ type: "claim", task_id: id });
      render();
    };
    li.append(info, btn);
    return li;
  }));
  $("empty").hidden = tasks.size > 0 || !socket;
  $("claimed").hidden = !claim;
  document.querySelector("main").classList.toggle("in-session", !!claim);
  if (claim) {
    $("claimed-url").textContent = claim.pageURL;
  } else {
    screen.hidden = true;
    screen.removeAttribute("src");
    $("screen-wait").hidden = false;
  }
  tick();
}

// tick refreshes the waited and time-left labels without rebuilding the list.
function tick() {
  for (const el of document.querySelectorAll(".waited")) {
    el.textContent = `Waiting ${Math.max(0, Math.floor((Date.now() - el.dataset.since) / 1000))}s`;
  }
  if (claim && Date.now() > claim.deadline) {
    claim = null; // the Task has Failed even if its notice was missed while offline
    render();
  }
  if (claim) {
    const left = Math.ceil((claim.deadline - Date.now()) / 1000);
    $("claimed-left").textContent = `${left}s left to solve`;
  }
}
setInterval(tick, 1000);

function showFrame(m) {
  // The page may be static, so the first frame can be the only one for a
  // while; keep it if it beats the claimed reply.
  if (claiming.has(m.task_id)) early.set(m.task_id, m);
  if (!claim || claim.id !== m.task_id) return;
  screen.src = `data:image/jpeg;base64,${m.data}`;
  screen.hidden = false;
  $("screen-wait").hidden = true;
  if (m.url && m.url !== claim.pageURL) {
    claim.pageURL = m.url;
    $("claimed-url").textContent = m.url;
  }
}

function usdc(units) { return (units / 1e6).toFixed(6).replace(/0+$/, "").replace(/\.$/, ""); }

function status(text) { $("status").textContent = text; }

function notice(text) {
  $("notice").textContent = text;
  $("notice").hidden = !text;
}

function short(addr) { return addr.length > 12 ? `${addr.slice(0, 4)}…${addr.slice(-4)}` : addr; }
