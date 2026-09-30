// Queue page: a Solver connects with their wallet, watches Pending Tasks
// arrive live, and Claims one. During the Session the Agent's page is shown
// live and the Solver's clicks are sent to the Bridge.
"use strict";

const $ = (id) => document.getElementById(id);
const tasks = new Map(); // task id -> { pageURL, since } where since is local ms when waited_ms was 0
const claiming = new Map(); // task id -> page URL, while a Claim is in flight
const early = new Map(); // task id -> frame that arrived before its claimed reply
let socket = null;
let wallet = "";
let claim = null; // { id, pageURL, deadline } for the Task this Solver holds
let retry = 0;

try { $("wallet").value = localStorage.getItem("overpass.wallet") || ""; } catch {}

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

// Pointer Events cover mouse and touch alike. Coordinates are sent normalized
// to 0–1 of the displayed frame, which shows the Agent's whole viewport.
const screen = $("screen");
screen.addEventListener("pointerdown", (e) => {
  e.preventDefault();
  screen.setPointerCapture(e.pointerId);
  pointer("down", e);
});
screen.addEventListener("pointerup", (e) => pointer("up", e));
screen.addEventListener("pointercancel", (e) => pointer("up", e));
screen.addEventListener("contextmenu", (e) => e.preventDefault());

function pointer(action, e) {
  if (!claim || screen.hidden) return;
  const r = screen.getBoundingClientRect();
  const clamp = (v) => Math.min(1, Math.max(0, v));
  send({
    type: "pointer",
    task_id: claim.id,
    action,
    x: clamp((e.clientX - r.left) / r.width),
    y: clamp((e.clientY - r.top) / r.height),
    t: e.timeStamp,
  });
}

function connect() {
  if (socket) socket.close();
  tasks.clear();
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
      claim = { id: m.task_id, pageURL: claiming.get(m.task_id) || "", deadline: Date.now() + m.solve_window_ms };
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
