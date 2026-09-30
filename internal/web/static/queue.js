// Queue page: a Solver connects with their wallet, watches Pending Tasks
// arrive live, and Claims one.
"use strict";

const $ = (id) => document.getElementById(id);
const tasks = new Map(); // task id -> { pageURL, since } where since is local ms when waited_ms was 0
const claiming = new Map(); // task id -> page URL, while a Claim is in flight
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
      notice("");
      break;
    case "claim_failed":
      claiming.delete(m.task_id);
      notice(m.error === "already_claimed" ? "Already claimed by another Solver." : `Claim failed: ${m.error}.`);
      break;
    case "task_failed":
      if (claim && claim.id === m.task_id) claim = null;
      notice(m.reason === "gave_up" ? "You gave up the Task." : "Solve window passed; the Task Failed.");
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
  if (claim) $("claimed-url").textContent = claim.pageURL;
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

function status(text) { $("status").textContent = text; }

function notice(text) {
  $("notice").textContent = text;
  $("notice").hidden = !text;
}

function short(addr) { return addr.length > 12 ? `${addr.slice(0, 4)}…${addr.slice(-4)}` : addr; }
