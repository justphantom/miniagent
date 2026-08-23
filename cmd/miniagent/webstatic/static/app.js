"use strict";

// miniagent -serve WebUI: auth gate, multi-session views, NDJSON streaming composer.
// ES modules, no build step; assets are embedded by webstatic (go:embed).
//
// Multi-view model (P3): #events is a viewport; each session owns a .view subtree managed by
// views.js. Switching views NEVER aborts a stream — turns are decoupled from connections
// server-side (D1) and keep rendering into their (hidden) view. The send button doubles as
// stop while the active view has a running turn; stopping goes through the stop API, and the
// local stream stays open to receive the partial result.

import { state, setKey, api, authHeaders, showSessionID, saveWorkdir, loadWorkdir, saveModel, loadModel, saveTheme, loadTheme, setVersion, refreshBudget, saveComposerAdv, loadComposerAdv, saveNavCollapsed, loadNavCollapsed, setStatusModel } from "./store.js";
import { appendUserPrompt, renderEvent, finishText, resetTransient } from "./events.js";
import { startEvents, attachLive } from "./live.js";
import { createView, byID, rekey, activate, activeView, dropView, eventsViewport, jumpToBottom } from "./views.js";
import { openConfigModal, closeConfigModal } from "./config.js";
import { attachDirPicker } from "./dirpicker.js";
import { toggleTrajectory, setActiveViewGetter, refreshPanel } from "./trajectory.js";

const $ = (id) => document.getElementById(id);
const runningKnown = new Set(); // session ids with a turn in flight (lifecycle feed)

// ---- auth gate ----

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

function showLogin() { $("login").classList.add("on"); $("app").classList.remove("on"); $("key-input").focus(); }function showApp() {
  $("login").classList.remove("on");
  $("app").classList.add("on");
  $("composer-adv").open = loadComposerAdv();
  document.body.classList.toggle("nav-collapsed", loadNavCollapsed());
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
$("traj-btn").addEventListener("click", () => toggleTrajectory());

// ---- auto-scroll: follow only when near bottom, otherwise show a jump button ----

const NEAR_BOTTOM = 80;

eventsViewport().addEventListener("scroll", () => {
  const el = eventsViewport();
  const v = activeView();
  if (v) v.stickBottom = el.scrollHeight - el.scrollTop - el.clientHeight < NEAR_BOTTOM;
  $("to-bottom").style.display = (v?.stickBottom === false) ? "block" : "none";
});
$("to-bottom").addEventListener("click", jumpToBottom);

// Hidden views mutating do not change scrollHeight (display:none), so one observer keyed to
// the active view's stickBottom covers all streams without cross-view jumps.
const scrollMo = new MutationObserver(() => {
  const v = activeView();
  if (v?.stickBottom) jumpToBottom();
});

// ---- composer & views ----

$("menu-btn").addEventListener("click", () => {
  if (matchMedia("(min-width: 800px)").matches) {
    const c = document.body.classList.toggle("nav-collapsed");
    saveNavCollapsed(c);
  } else {
    document.body.classList.toggle("nav-open");
  }
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") document.body.classList.remove("nav-open");
});
$("overlay").addEventListener("click", () => document.body.classList.remove("nav-open"));

$("composer-adv").addEventListener("toggle", (e) => saveComposerAdv(e.target.open));

$("new-chat").addEventListener("click", () => {
  activate(createView(""));
  updateHeader();
  document.body.classList.remove("nav-open");
  if (!isTouch) { $("prompt").focus(); }
});

$("config-btn").addEventListener("click", () => {
  openConfigModal();
  document.body.classList.remove("nav-open");
});
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
  el.style.height = Math.min(el.scrollHeight, 200) + "px";
});

// Send doubles as stop while the active view runs a turn. Stopping calls the stop API — the
// local stream stays attached to receive the saved partial result.
$("send").addEventListener("click", () => {
  const v = activeView();
  if (v?.running) { stopTurn(v); return; }
  send();
});

async function send() {
  const v = activeView();
  if (!v || v.sending) return;
  const prompt = $("prompt").value.trim();
  const workdir = $("workdir").value.trim();
  if (!prompt) { inlineHint("请输入内容"); return; }
  if (!workdir) { inlineHint("请输入工作目录"); return; }
  const sel = $("model");
  const opt = sel.selectedOptions[0];
  const body = { prompt, workdir, session: v.id, provider: opt?.dataset.provider || "", model: opt?.dataset.model || "", thinking: opt?.dataset.thinking || "" };

  v.sending = true;
  v.running = true;
  v.lastPrompt = prompt;
  v.turnStartTs = 0;
  v.abort = new AbortController();
  const gen = ++v.gen; // M8: drop events superseded by a newer stream on this view
  updateComposer();
  startWait();
  appendUserPrompt(v, prompt);
  $("prompt").value = "";
  $("prompt").style.height = "auto";
  $("prompt").focus();
  saveWorkdir(workdir);
  scrollMo.observe(eventsViewport(), { childList: true, subtree: true, characterData: true });

  let sawTerminal = false;
  try {
    const resp = await fetch("/api/turn", {
      method: "POST",
      headers: { "Content-Type": "application/json", ...authHeaders() },
      body: JSON.stringify(body),
      signal: v.abort.signal,
    });
    if (resp.status === 409) {
      // Another window (or this one) is already driving this session: become a spectator.
      v.sending = false;
      finishText(v);
      attachSpectator(v, true);
      return;
    }
    if (!resp.ok) {
      let msg = resp.statusText;
      try { msg = (await resp.json()).error || msg; } catch { /* non-JSON error body */ }
      throw new Error(msg);
    }
    const reader = resp.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let nl;
      while ((nl = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, nl).trim();
        buf = buf.slice(nl + 1);
        if (!line) continue;
        try {
          const ev = JSON.parse(line);
          if (v.gen !== gen) return; // superseded stream — stop painting
          if (ev.type === "session" && ev.id) rekey(v, ev.id);
          if (ev.type === "result" || ev.type === "error") sawTerminal = true;
          stopWait();
          renderEvent(v, ev);
          updateHeader();
        } catch (e) { console.log("bad ndjson line", line, e); }
      }
    }
    if (!sawTerminal) {
      renderEvent(v, { type: "error", error: "连接中断：流意外结束（会话已保存已执行部分，可点击重试续跑）", ts: Date.now() });
    }
  } catch (e) {
    if (e.name === "AbortError") {
      renderEvent(v, { type: "error", error: "已停止（会话已保存已执行部分）", ts: Date.now() });
    } else {
      renderEvent(v, { type: "error", error: "请求失败：" + e.message, ts: Date.now() });
    }
  } finally {
    stopWait();
    if (v.gen === gen) finishText(v); // a superseded stream's view is left alone
    v.sending = false;
    if (!v.liveDetach) v.running = false; // spectator attach keeps the running state
    v.abort = null;
    updateComposer();
    scrollMo.disconnect();
    loadSessions();
  }
}

// stopTurn cancels the server-side turn (D1): the id is known from the early session event;
// the local stream then winds down on its own with the saved partial result.
async function stopTurn(v) {
  if (!v.id) { v.abort?.abort(); return; } // pre-session-event window: abort locally, turn id unknown yet
  try { await api(`/api/sessions/${encodeURIComponent(v.id)}/stop`, { method: "POST" }); }
  catch (e) { inlineHint("停止失败：" + e.message); }
}

// attachSpectator follows a running turn of ANOTHER window into this view (409 upgrade or
// lifecycle-driven attach). The replay buffer is delivered from event zero, so the view
// rebuilds cleanly (D3).
function attachSpectator(v, withHint) {
  if (!v.id || v.liveDetach || v.sending) return;
  if (withHint) {
    const hint = document.createElement("div");
    hint.className = "ev spectator muted";
    hint.textContent = "该会话有进行中的轮次（其他窗口），已进入旁观…";
    v.dom.appendChild(hint);
  }
  v.running = true;
  v.gen++;
  const gen = v.gen;
  v.liveDetach = attachLive(v.id, {
    event: (ev) => {
      if (v.gen !== gen) return;
      if (ev.type === "session" && ev.id) rekey(v, ev.id);
      renderEvent(v, ev);
      updateHeader();
    },
    end: () => {
      v.liveDetach = null;
      v.running = false;
      finishText(v);
      updateComposer();
      loadSessions();
    },
  });
  updateComposer();
}

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

// ---- header / composer reflect the ACTIVE view ----

function updateHeader() {
  const v = activeView();
  document.title = v?.id ? `miniagent · ${v.id}` : "miniagent";
  showSessionID(v);
  updateComposer();
}

function updateComposer() {
  const v = activeView();
  const busy = !!v?.running;
  $("send").textContent = busy ? "停止" : "发送";
  $("send").classList.toggle("danger", busy);
  $("prompt").disabled = false;
  $("workdir").disabled = false;
  $("model").disabled = false;
}

// Inline validation hint (replaces alert()): transient, next to the composer.
function inlineHint(msg) {
  const el = $("wait");
  el.hidden = false;
  el.textContent = msg;
  el.classList.add("msg-error");
  setTimeout(() => { el.classList.remove("msg-error"); el.hidden = true; el.textContent = ""; }, 2500);
}

// Inline confirm dialog (replaces native confirm()): resolves true on confirm, false otherwise.
function confirmInline(msg, okText) {
  const overlay = document.createElement("div");
  overlay.setAttribute("role", "dialog");
  overlay.setAttribute("aria-modal", "true");
  overlay.className = "confirm-overlay";
  const box = document.createElement("div");
  box.className = "confirm-box";
  const p = document.createElement("p");
  p.textContent = msg;
  const btnOk = document.createElement("button");
  btnOk.textContent = okText;
  btnOk.className = "confirm-ok";
  const btnCancel = document.createElement("button");
  btnCancel.textContent = "取消";
  btnCancel.className = "confirm-cancel";
  const row = document.createElement("div");
  row.className = "confirm-row";
  row.append(btnCancel, btnOk);
  box.append(p, row);
  overlay.append(box);
  document.body.append(overlay);
  btnCancel.focus();
  const focusables = [btnCancel, btnOk];
  overlay.addEventListener("keydown", (e) => {
    if (e.key !== "Tab") return;
    const last = focusables[focusables.length - 1], first = focusables[0];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  });
  return new Promise(resolve => {
    const close = (val) => { overlay.remove(); resolve(val); };
    btnOk.addEventListener("click", () => close(true));
    btnCancel.addEventListener("click", () => close(false));
    overlay.addEventListener("click", (e) => { if (e.target === overlay) close(false); });
    overlay.addEventListener("keydown", (e) => {
      if (e.key === "Escape") close(false);
      else if (e.key === "Enter" && document.activeElement === btnOk) close(true);
    });
  });
}

// Wait indicator for non-streaming configs: between send and the first event the UI would
// otherwise look dead (no deltas arrive until the terminal result).
let waitTimer = 0;
function startWait() {
  const el = $("wait");
  let dots = 0;
  el.hidden = false;
  el.classList.remove("msg-error");
  waitTimer = setInterval(() => { dots = (dots + 1) % 4; el.textContent = `等待响应${".".repeat(dots)}`; }, 400);
}
function stopWait() {
  if (!waitTimer) return;
  clearInterval(waitTimer);
  waitTimer = 0;
  $("wait").hidden = true;
}

// ---- models / sessions ----

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

let sessionMeta = {}; // id → { workdir, model, running, ... } captured from the session list

// N5: the session list carries three states (loading/empty/error-with-retry).
async function loadSessions() {
  const box = $("session-list");
  box.innerHTML = "";
  const hint = document.createElement("div");
  hint.className = "muted sess-empty";
  hint.textContent = "加载中…";
  box.appendChild(hint);
  try {
    const r = await api("/api/sessions");
    const list = await r.json();
    if (!Array.isArray(list)) return;
    box.innerHTML = "";
    if (list.length === 0) {
      hint.textContent = "暂无会话，点击「＋ 新会话」开始";
      box.appendChild(hint);
      return;
    }
    for (const s of list) {
      sessionMeta[s.id] = s;
      const b = document.createElement("button");
      b.className = "sess-item" + (s.id === activeView()?.id ? " active" : "") + (s.running ? " running" : "");
      b.type = "button";
      const top = document.createElement("div");
      top.textContent = s.model || s.id;
      top.title = s.model || s.id;
      const sid = document.createElement("div");
      sid.className = "sid";
      const sidText = [s.id, s.workdir || "", s.created ? new Date(s.created).toLocaleString() : ""].filter(Boolean).join(" · ");
      sid.textContent = sidText;
      sid.title = sidText;
      b.appendChild(top); b.appendChild(sid);
      if (s.preview) {
        const pv = document.createElement("div");
        pv.className = "preview";
        pv.textContent = s.preview;
        pv.title = s.preview;
        b.appendChild(pv);
      }
      const del = document.createElement("button");
      del.type = "button";
      del.className = "sess-del ghost";
      del.textContent = "✕";
      del.title = "删除会话";
      del.setAttribute("aria-label", "删除会话");
      del.tabIndex = -1;
      del.addEventListener("click", (e) => { e.stopPropagation(); deleteSession(s.id); });
      del.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.stopPropagation(); deleteSession(s.id); } });
      b.appendChild(del);
      b.addEventListener("click", () => openSession(s.id));
      box.appendChild(b);
    }
  } catch (e) {
    hint.textContent = `加载失败：${e.message}（点击重试）`;
    hint.style.cursor = "pointer";
    hint.addEventListener("click", () => loadSessions());
  }
}

async function deleteSession(id) {
  if (await confirmInline(`删除会话 ${id}？此操作不可恢复。`, "删除")) {
    try {
      await api(`/api/sessions/${encodeURIComponent(id)}`, { method: "DELETE" });
      const v = byID(id);
      if (v) {
        dropView(v);
        if (!activeView()) activate(createView(""));
      }
      loadSessions();
    } catch (e) { inlineHint("删除失败：" + e.message); }
  }
}

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
  document.body.classList.remove("nav-open");
  if (!isTouch) { $("prompt").focus(); }
  if (fresh) await loadReplay(v);
  ensureEmptyState(v);
  refreshPanel(); // F5: refresh trajectory panel after switching views (it may be open)
  if ((sessionMeta[id]?.running || runningKnown.has(id)) && !v.sending && !v.liveDetach) {
    attachSpectator(v, true);
  }
}

// loadReplay streams the persisted history into the view (tail-capped server-side). The
// session event fills workdir when the input is still empty (N14: explicit input wins).
function ensureEmptyState(v) {
  if (v.dom.children.length === 0 && !v.sending) {
    const hint = document.createElement("div");
    hint.className = "ev empty-state muted";
    hint.textContent = "暂无对话，输入任务开始";
    v.dom.appendChild(hint);
  }
}

async function loadReplay(v) {
  const gen = ++v.gen;
  v.usage.budget = 0;
  v.usage.steps.length = 0;
  v.trajectory.order.length = 0;
  v.trajectory.steps.clear();
  v.curStep = 0;
  v.workdir = sessionMeta[v.id]?.workdir || "";
  if (v.workdir && !$("workdir").value.trim()) { $("workdir").value = v.workdir; saveWorkdir(v.workdir); }
  let workdirFilled = !!v.workdir;
  scrollMo.observe(eventsViewport(), { childList: true, subtree: true, characterData: true });
  try {
    const r = await api(`/api/sessions/${encodeURIComponent(v.id)}`);
    const reader = r.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let nl;
      while ((nl = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, nl).trim();
        buf = buf.slice(nl + 1);
        if (!line) continue;
        try {
          const ev = JSON.parse(line);
          if (v.gen !== gen) { scrollMo.disconnect(); return; } // superseded: view rebuilt/stream took over
          if (!workdirFilled && ev.type === "session" && ev.workdir && !$("workdir").value.trim()) {
            $("workdir").value = ev.workdir;
            saveWorkdir(ev.workdir);
            workdirFilled = true;
          }
          renderEvent(v, ev);
        } catch { /* skip bad lines */ }
      }
    }
    if (v.gen === gen) { finishText(v); updateHeader(); refreshPanel(); }
  } catch (e) { console.log("replay failed", e); }
  scrollMo.disconnect();
}

boot();
