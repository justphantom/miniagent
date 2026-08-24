"use strict";

// miniagent -serve WebUI entry: auth gate, composition root, DOM wiring.
// ES modules, no build step; assets are embedded by webstatic (go:embed).
// Feature modules: events.js (NDJSON render), send.js (turn stream lifecycle),
// sessions.js (session list/replay), ui.js (header/composer/hint/dialog),
// trajectory.js/panel.js/views.js/store.js/config.js/dirpicker.js.
//
// Multi-view model (P3): #events is a viewport; each session owns a .view subtree managed by
// views.js. Switching views NEVER aborts a stream — turns are decoupled from connections
// server-side (D1) and keep rendering into their (hidden) view. The send button doubles as
// stop while the active view has a running turn; stopping goes through the stop API, and the
// local stream stays open to receive the partial result.

import { state, setKey, api, authHeaders, loadWorkdir, saveWorkdir, loadModel, saveModel, loadTheme, saveTheme, setVersion, refreshBudget, loadComposerAdv, saveComposerAdv, setStatusModel } from "./store.js";
import { finishText } from "./events.js";
import { startEvents } from "./live.js";
import { createView, byID, activate, activeView, dropView, jumpToBottom, eventsViewport } from "./views.js";
import { openConfigModal, closeConfigModal } from "./config.js";
import { attachDirPicker } from "./dirpicker.js";
import { showTrajectory, hideTrajectory, setOnTrajectoryClose, setActiveViewGetter, refreshPanel } from "./trajectory.js";
import { initPanel, togglePanel, closePanel, filterSessions } from "./panel.js";
import { $, updateHeader, updateComposer } from "./ui.js";
import { send, stopTurn, attachSpectator, runningKnown } from "./send.js";
import { loadSessions, ensureEmptyState, loadReplay, getMeta } from "./sessions.js";

async function boot() {
  applyTheme(loadTheme());
  try {
    const r = await fetch("/api/whoami");
    const w = await r.json();
    if (w.version) setVersion(w.version);
    if (!w.auth_required) { showApp(); return; }
  } catch { /* fall through to key check */ }
  if (!state.key) { showLogin(); return; }
  try {
    const probe = await fetch("/api/sessions", { headers: authHeaders() });
    if (probe.ok) { showApp(); return; }
  } catch { /* offline probe falls through to login */ }
  showLogin();
}

function showLogin() { $("login").classList.add("on"); $("app").classList.remove("on"); $("key-input").focus(); }

function showApp() {
  $("login").classList.remove("on");
  $("app").classList.add("on");
  $("composer-adv").open = loadComposerAdv();
  const savedWd = loadWorkdir();
  if (savedWd) $("workdir").value = savedWd;
  activate(createView("")); // the initial draft view
  updateHeader();
  loadModels(); loadSessions();
  startLifecycleSync();
  refreshBudget();
  setActiveViewGetter(activeView);
  attachDirPicker({ onPick: (p) => { $("workdir").value = p; saveWorkdir(p); } });
  ensureEmptyState(activeView());
  $("prompt").focus();
}

$("login-form").addEventListener("submit", (e) => {
  e.preventDefault();
  const v = $("key-input").value.trim();
  if (!v) return;
  state.key = v;
  fetch("/api/sessions", { headers: authHeaders() }).then((r) => {
    if (r.ok) { setKey(v); showApp(); }
    else { $("login-err").textContent = "key 无效或服务不可达"; }
  }).catch(() => { $("login-err").textContent = "无法连接服务"; });
});
$("logout").addEventListener("click", () => { setKey(""); location.reload(); });

const themeBtn = $("theme-btn");
// N9: the theme button doubles as the current-theme indicator — ◐ dark / ◑ light.
function applyTheme(t) {
  document.documentElement.dataset.theme = t || "";
  saveTheme(t || "");
  const light = t === "light";
  themeBtn.textContent = light ? "◑" : "◐";
  themeBtn.title = light ? "切换到暗色" : "切换到亮色";
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.content = getComputedStyle(document.documentElement).getPropertyValue("--bg").trim();
}
themeBtn.addEventListener("click", () => applyTheme(document.documentElement.dataset.theme === "light" ? "" : "light"));

// ---- trajectory toggle ----
function switchTab(name) {
  for (const t of document.querySelectorAll("#tabs .tab")) t.classList.toggle("active", t.dataset.tab === name);
  if (name === "trajectory") {
    showTrajectory();
    $("to-bottom").style.display = "none";
  } else {
    hideTrajectory();
  }
}
for (const t of document.querySelectorAll("#tabs .tab")) {
  t.addEventListener("click", () => switchTab(t.dataset.tab));
}
setOnTrajectoryClose(() => switchTab("chat"));

// ---- auto-scroll: follow only when near bottom, otherwise show a jump button ----
const NEAR_BOTTOM = 80;
eventsViewport().addEventListener("scroll", () => {
  const el = eventsViewport();
  const v = activeView();
  if (v) v.stickBottom = el.scrollHeight - el.scrollTop - el.clientHeight < NEAR_BOTTOM;
  $("to-bottom").style.display = (v?.stickBottom === false) ? "block" : "none";
});
$("to-bottom").addEventListener("click", jumpToBottom);

// ---- icon bar / slide panel / tabs ----
$("menu-btn").addEventListener("click", () => document.body.classList.toggle("iconbar-open"));
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") document.body.classList.remove("iconbar-open");
});

$("icon-sessions").addEventListener("click", () => {
  togglePanel();
  document.body.classList.remove("iconbar-open");
});
$("icon-new").addEventListener("click", () => {
  activate(createView(""));
  updateHeader();
  closePanel();
  document.body.classList.remove("iconbar-open");
  switchTab("chat");
  if (!isTouch) { $("prompt").focus(); }
});
$("icon-settings").addEventListener("click", () => {
  openConfigModal();
  document.body.classList.remove("iconbar-open");
});
initPanel();
$("session-search").addEventListener("input", (e) => filterSessions(e.target.value));

$("composer-adv").addEventListener("toggle", (e) => saveComposerAdv(e.target.open));
$("cfg-close").addEventListener("click", closeConfigModal);
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && !$("config-modal").hidden) closeConfigModal();
});
$("config-modal").addEventListener("click", (e) => { if (e.target === $("config-modal")) closeConfigModal(); });

// Enter sends on desktop (Shift+Enter = newline); mobile keeps Enter = newline.
const isTouch = matchMedia("(hover: none)").matches;
$("prompt").addEventListener("keydown", (e) => {
  if (!isTouch && e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); $("send").click(); }
});
$("prompt").addEventListener("input", () => {
  const el = $("prompt");
  el.style.height = "auto";
  el.style.height = Math.min(el.scrollHeight, 120) + "px";
});

// Send doubles as stop while the active view runs a turn. Stopping calls the stop API — the
// local stream stays attached to receive the saved partial result.
$("send").addEventListener("click", () => {
  const v = activeView();
  if (v?.running) { stopTurn(v); return; }
  send();
});

// ---- cross-browser sync: lifecycle feed drives the list, running dots and spectator attach.
let lifeStop = null;
let listRefreshTimer = 0;
function startLifecycleSync() {
  if (lifeStop) return;
  lifeStop = startEvents(onLifeEvent);
}

function onLifeEvent(ev) {
  const v = byID(ev.session);
  if (ev.type === "turn_started") {
    runningKnown.add(ev.session);
    if (v && !v.sending) {
      v.running = true;
      if (activeView() === v && !v.liveDetach) attachSpectator(v, true);
      updateComposer();
    }
  } else if (ev.type === "turn_finished") {
    runningKnown.delete(ev.session);
    if (v && !v.sending) {
      v.running = false;
      v.liveDetach?.();
      v.liveDetach = null;
      finishText(v);
      updateComposer();
    }
  } else if (ev.type === "session_deleted") {
    runningKnown.delete(ev.session);
    if (v) {
      dropView(v);
      if (!activeView()) activate(createView(""));
      updateHeader();
    }
  }
  if (listRefreshTimer) return;
  listRefreshTimer = setTimeout(() => { listRefreshTimer = 0; loadSessions(); }, 300);
}

// ---- models ----
async function loadModels() {
  try {
    const r = await api("/api/models");
    const models = await r.json();
    const sel = $("model");
    sel.innerHTML = "";
    const saved = loadModel();
    for (const m of models) {
      const o = document.createElement("option");
      o.textContent = `${m.provider}/${m.model}`;
      o.dataset.provider = m.provider;
      o.dataset.model = m.model;
      o.dataset.thinking = m.thinking || "";
      sel.appendChild(o);
      if (saved === `${m.provider}/${m.model}`) o.selected = true;
    }
    sel.addEventListener("change", () => {
      const opt = sel.selectedOptions[0];
      const v = opt?.dataset.model ? `${opt.dataset.provider}/${opt.dataset.model}` : "";
      saveModel(v);
      setStatusModel(v);
    });
    setStatusModel(saved || "默认模型");
  } catch { /* dropdown stays default */ }
}

// ---- open session: rebuild from replay (kept here: needs attachSpectator + send.js hook) ----
// openSession switches to the session's view, building it on first open. Switching NEVER
// aborts an in-flight stream (D1). On first open the persisted history replays first, THEN a
// running turn attaches as spectator — the live stream replays its buffer from event zero,
// so replay-then-attach yields the full timeline without interleaving.
async function openSession(id) {
  let v = byID(id);
  const fresh = !v;
  if (fresh) v = createView(id);
  activate(v);
  updateHeader();
  closePanel();
  document.body.classList.remove("iconbar-open");
  if (!isTouch) { $("prompt").focus(); }
  if (fresh) await loadReplay(v);
  ensureEmptyState(v);
  refreshPanel(); // F5: refresh trajectory panel after switching views (it may be open)
  if ((getMeta(id)?.running || runningKnown.has(id)) && !v.sending && !v.liveDetach) {
    attachSpectator(v, true);
  }
}

// Delegate session clicks from sessions.js (avoids a sessions→app import cycle).
document.addEventListener("open-session", (e) => openSession(e.detail));

boot();
