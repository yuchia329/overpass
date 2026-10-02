// Queue page: a Solver connects with their wallet, watches Pending Tasks
// arrive live, and Claims one. During the Session the Agent's page is shown
// live and the Solver's pointer, wheel and keyboard input is sent to the Bridge, over a
// direct WebRTC connection when the Solver allows one and it comes up, and
// through the backend otherwise.
"use strict";

const $ = (id) => document.getElementById(id);
const tasks = new Map(); // task id -> { pageURL, obstacle, since } where since is local ms when waited_ms was 0
const claiming = new Map(); // task id -> { pageURL, obstacle }, while a Claim is in flight
const early = new Map(); // task id -> frame that arrived before its claimed reply
let socket = null;
let wallet = "";
// { id, pageURL, obstacle, deadline, iceServers, checking } for the Task this
// Solver holds; checking while the Agent checks the Solver's Done.
let claim = null;
let peer = null; // { taskId, pc, channel, token, ready, frame, timer }: the direct connection for the claim
const exposed = new Set(); // ids of Tasks whose Agent was sent this device's addresses
let blobURL = null; // the object URL on screen, released when replaced
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

try { $("direct").checked = localStorage.getItem("overpass.direct") !== "off"; } catch {}
$("direct").addEventListener("change", () => {
  try { localStorage.setItem("overpass.direct", $("direct").checked ? "on" : "off"); } catch {}
  if (!$("direct").checked) closePeer();
  else if (claim && !peer) startPeer();
  renderLink();
});

$("give-up").addEventListener("click", () => {
  if (claim) send({ type: "give_up", task_id: claim.id });
});

// Done asks the Agent to check the obstacle. It answers with task_solved, or
// with not_cleared and the page stays the Solver's.
$("done").addEventListener("click", () => {
  if (!claim || claim.checking) return;
  claim.checking = true;
  send({ type: "done", task_id: claim.id });
  notice("The Agent is checking…");
  render();
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
    if (at) sendInput({ type: "wheel", task_id: claim.id, ...at, dx: pendingWheel.dx, dy: pendingWheel.dy, t: pendingWheel.e.timeStamp });
  }
  pendingMove = pendingWheel = null;
}

// pointer sends a pointer event and reports whether it was sent.
function pointer(action, e) {
  const at = position(e);
  if (!at) return false;
  sendInput({ type: "pointer", task_id: claim.id, action, ...at, t: e.timeStamp });
  return true;
}

// Keyboard. Typed text and named keys go to whatever the Solver last clicked
// into on the Agent's page. On a phone the keys field brings up the on-screen
// keyboard; on a computer, typing anywhere on this page works too. Shortcuts
// with Ctrl, Cmd or Alt stay with this browser.
const keysField = $("keys");
const KEYS = new Set([ // as the backend allows
  "Enter", "Tab", "Backspace", "Delete", "Escape",
  "ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown",
  "Home", "End", "PageUp", "PageDown",
]);
const MAX_TEXT = 64; // characters per text event, as the backend allows

// sendText sends text in pieces the backend accepts, turning line breaks into
// Enter and dropping other control characters.
function sendText(text) {
  if (!claim) return;
  flushInput(); // keep order with a pending move
  text.split(/\r\n|\r|\n/).forEach((line, i) => {
    if (i > 0) sendKey("Enter");
    const chars = [...line.replace(/\p{Cc}/gu, "")];
    for (let at = 0; at < chars.length; at += MAX_TEXT) {
      sendInput({ type: "text", task_id: claim.id, text: chars.slice(at, at + MAX_TEXT).join(""), t: performance.now() });
    }
  });
}

function sendKey(key) {
  if (!claim) return;
  flushInput();
  sendInput({ type: "key", task_id: claim.id, key, t: performance.now() });
}

// The keys field sends what is typed into it and empties itself, except while
// an input method (or a phone's autocorrect) is composing a word.
let composing = false;
function sendField() {
  if (composing || !keysField.value) return;
  sendText(keysField.value);
  keysField.value = "";
}
keysField.addEventListener("compositionstart", () => { composing = true; });
keysField.addEventListener("compositionend", () => { composing = false; sendField(); });
keysField.addEventListener("input", (e) => {
  if (e.inputType === "deleteContentBackward" && !composing) sendKey("Backspace");
  sendField();
});
keysField.addEventListener("keydown", (e) => {
  if (e.isComposing || composing || !KEYS.has(e.key)) return;
  e.preventDefault();
  sendField();
  sendKey(e.key);
});

const typingElsewhere = (e) => e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement;
document.addEventListener("keydown", (e) => {
  if (!claim || screen.hidden || typingElsewhere(e) || e.ctrlKey || e.metaKey || e.altKey || e.isComposing) return;
  if ([...e.key].length === 1) sendText(e.key); // a printable character
  else if (KEYS.has(e.key)) sendKey(e.key);
  else return;
  e.preventDefault(); // e.g. Space and arrows would scroll this page
});
document.addEventListener("paste", (e) => {
  if (!claim || screen.hidden || typingElsewhere(e)) return;
  e.preventDefault();
  sendText(e.clipboardData.getData("text"));
});

// sendInput sends over the direct connection while it is up. The Bridge
// applies input from both paths in the order it arrives.
function sendInput(msg) {
  if (peer && peer.ready && peer.channel.readyState === "open") peer.channel.send(JSON.stringify(msg));
  else send(msg);
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
      tasks.set(m.task_id, { pageURL: m.page_url, obstacle: m.obstacle || "", since: Date.now() - m.waited_ms });
      break;
    case "task_removed":
      tasks.delete(m.task_id);
      break;
    case "claimed":
      // A Claim just won carries solve_window_ms; one resumed on reconnect
      // carries solve_left_ms, its page URL and its obstacle.
      claim = {
        id: m.task_id,
        pageURL: m.page_url || claiming.get(m.task_id)?.pageURL || "",
        obstacle: m.obstacle ?? claiming.get(m.task_id)?.obstacle ?? "",
        deadline: Date.now() + (m.solve_left_ms ?? m.solve_window_ms),
        iceServers: m.ice_servers || [],
      };
      claiming.delete(m.task_id);
      startPeer();
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
    case "not_cleared":
      if (!claim || claim.id !== m.task_id) return;
      claim.checking = false;
      notice("The Agent still sees the obstacle. Keep going, then tap Done again.");
      break;
    case "task_solved":
      if (claim && claim.id === m.task_id) claim = null;
      notice(`Solved! Earning of ${usdc(m.earning)} USDC recorded.`);
      break;
    case "rtc_answer":
      if (peer && peer.taskId === m.task_id && !peer.token) {
        peer.token = m.peer_token;
        peer.pc.setRemoteDescription({ type: "answer", sdp: m.sdp }).catch(closePeer);
      }
      return;
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
    const obstacle = document.createElement("div");
    obstacle.className = "obstacle";
    obstacle.textContent = t.obstacle;
    obstacle.hidden = !t.obstacle;
    const waited = document.createElement("div");
    waited.className = "waited";
    waited.dataset.since = t.since;
    info.append(url, obstacle, waited);
    const btn = document.createElement("button");
    btn.textContent = "Claim";
    btn.disabled = claiming.has(id) || claim !== null;
    btn.onclick = () => {
      claiming.set(id, { pageURL: t.pageURL, obstacle: t.obstacle });
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
    $("claimed-obstacle").textContent = `Your job: ${claim.obstacle || "clear the check that blocks the Agent."}`;
    $("done").disabled = !!claim.checking;
  } else {
    closePeer();
    screen.hidden = true;
    keysField.hidden = $("keys-hint").hidden = true;
    keysField.value = "";
    keysField.blur();
    showImage(null);
    $("screen-wait").hidden = false;
  }
  renderLink();
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
  if (peer && peer.ready) return; // a relayed frame is older than the direct ones
  showImage(`data:image/jpeg;base64,${m.data}`);
  showURL(m.url);
}

// showImage puts src on screen: a data URL, a Blob, or null to clear it.
function showImage(src) {
  const old = blobURL;
  blobURL = src instanceof Blob ? URL.createObjectURL(src) : null;
  if (src === null) screen.removeAttribute("src");
  else {
    screen.src = blobURL || src;
    screen.hidden = false;
    keysField.hidden = $("keys-hint").hidden = false;
    $("screen-wait").hidden = true;
  }
  if (old) URL.revokeObjectURL(old);
}

function showURL(url) {
  if (!claim || !url || url === claim.pageURL) return;
  claim.pageURL = url;
  $("claimed-url").textContent = url;
}

// Direct connection. After a Claim the page offers the Bridge a WebRTC data
// channel through the backend and presents the peer token the backend
// returns with the answer. Once the Bridge welcomes it, frames and input
// skip the backend; if it never comes up or drops, they go through the
// backend again.
const DIRECT_TIMEOUT_MS = 10000;
const GATHER_TIMEOUT_MS = 3000;

function startPeer() {
  closePeer();
  if (!claim || !$("direct").checked || typeof RTCPeerConnection === "undefined") return;
  const pc = new RTCPeerConnection({ iceServers: claim.iceServers });
  const channel = pc.createDataChannel("overpass");
  channel.binaryType = "arraybuffer";
  const p = { taskId: claim.id, pc, channel, token: null, ready: false, frame: null, timer: 0 };
  peer = p;
  const drop = () => { if (peer === p) closePeer(); };
  p.timer = setTimeout(() => { if (!p.ready) drop(); }, DIRECT_TIMEOUT_MS);
  channel.onopen = () => channel.send(JSON.stringify({ type: "hello", peer_token: p.token }));
  channel.onclose = drop;
  channel.onmessage = (e) => { if (peer === p) peerMessage(p, e.data); };
  pc.onconnectionstatechange = () => {
    if (["disconnected", "failed", "closed"].includes(pc.connectionState)) drop();
  };
  (async () => {
    await pc.setLocalDescription(await pc.createOffer());
    await gathered(pc);
    if (peer !== p) return;
    exposed.add(p.taskId); // the offer carries this device's addresses
    send({ type: "rtc_offer", task_id: p.taskId, sdp: pc.localDescription.sdp });
    renderLink();
  })().catch(drop);
}

function closePeer() {
  const p = peer;
  if (!p) return;
  peer = null;
  clearTimeout(p.timer);
  p.channel.close();
  // The channel's close must reach the Bridge before the connection goes.
  setTimeout(() => p.pc.close(), 200);
  renderLink();
}

// A frame arrives as a JSON header followed by its JPEG bytes in pieces.
function peerMessage(p, data) {
  if (typeof data !== "string") {
    const f = p.frame;
    if (!f) return;
    f.parts.push(data);
    f.got += data.byteLength;
    if (f.got < f.size) return;
    p.frame = null;
    if (claim && claim.id === p.taskId) showImage(new Blob(f.parts, { type: "image/jpeg" }));
    return;
  }
  const m = JSON.parse(data);
  switch (m.type) {
    case "welcome":
      p.ready = true;
      clearTimeout(p.timer);
      renderLink();
      break;
    case "frame":
      p.frame = { size: m.size, parts: [], got: 0 };
      break;
    case "url":
      showURL(m.url);
      break;
  }
}

// The offer goes out whole, candidates included; an unreachable STUN or
// TURN server costs only its candidates.
function gathered(pc) {
  return new Promise((resolve) => {
    if (pc.iceGatheringState === "complete") return resolve();
    setTimeout(resolve, GATHER_TIMEOUT_MS);
    pc.addEventListener("icegatheringstatechange", () => {
      if (pc.iceGatheringState === "complete") resolve();
    });
  });
}

// renderLink tells the Solver how the Session reaches them and whether the
// Agent can see their IP address.
function renderLink() {
  let text = "";
  if (claim && peer && peer.ready) text = "Direct connection. The Agent can see your IP address.";
  else if (claim && peer) text = "Connecting directly. The Agent can see your IP address.";
  else if (claim && exposed.has(claim.id)) text = "Relayed through Overpass. The Agent may have seen your IP address while connecting directly.";
  else if (claim) text = "Relayed through Overpass. The Agent cannot see your IP address.";
  $("link").textContent = text;
}

function usdc(units) { return (units / 1e6).toFixed(6).replace(/0+$/, "").replace(/\.$/, ""); }

function status(text) { $("status").textContent = text; }

function notice(text) {
  $("notice").textContent = text;
  $("notice").hidden = !text;
}

function short(addr) { return addr.length > 12 ? `${addr.slice(0, 4)}…${addr.slice(-4)}` : addr; }
