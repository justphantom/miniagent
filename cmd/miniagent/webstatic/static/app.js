"use strict";

// miniagent -serve WebUI: auth gate, session list, NDJSON streaming composer.
// ES modules, no build step; assets are embedded by webstatic (go:embed).

import { state, setKey, api, authHeaders, showSessionID, resetTokenCount, saveWorkdir, loadWorkdir, saveModel, loadModel, saveTheme, loadTheme, setVersion, setModelBadge } from "./store.js";
import { appendUserPrompt, renderEvent, finishText, resetTransient } from "./events.js";

const $ = (id) => document.getElementById(id);
state.lastPrompt = ""; // last sent prompt, used by the error retry button

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

function showLogin() { $("login").style.display = "flex"; $("app").style.display = "none"; $("key-input").focus(); }
function showApp() {
  $("login").style.display = "none"; $("app").style.display = "flex";
  const savedWd = loadWorkdir();
  if (savedWd) $("workdir").value = savedWd;
  loadModels(); loadSessions(); $("prompt").focus();
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
// N9: the theme button doubles as the current-theme indicator — ◐ dark / ◑ light, with a title
// saying what a click does. Previously it gave no state feedback (always ◐, always "切换主题").
function applyTheme(t) {
  document.documentElement.dataset.theme = t || "";
  saveTheme(t || "");
  const light = t === "light";
  themeBtn.textContent = light ? "◑" : "◐";
  themeBtn.title = light ? "切换到暗色" : "切换到亮色";
}
themeBtn.addEventListener("click", () => applyTheme(document.documentElement.dataset.theme === "light" ? "" : "light"));

// ---- auto-scroll: follow only when near bottom, otherwise show a jump button ----

let stickBottom = true;
const NEAR_BOTTOM = 80;
let generation = 0; // M8: snapshot per send(); new-chat/openSession bump it so stale stream events drop

function eventsEl() { return $("events"); }

eventsEl().addEventListener("scroll", () => {
  const el = eventsEl();
  stickBottom = el.scrollHeight - el.scrollTop - el.clientHeight < NEAR_BOTTOM;
  $("to-bottom").style.display = stickBottom ? "none" : "block";
});
$("to-bottom").addEventListener("click", () => { eventsEl().scrollTop = eventsEl().scrollHeight; });

const scrollMo = new MutationObserver(() => { if (stickBottom) eventsEl().scrollTop = eventsEl().scrollHeight; });

// ---- composer ----

$("menu-btn").addEventListener("click", () => document.body.classList.toggle("nav-open"));
// Escape closes the drawer (L9 keyboard operability).
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") document.body.classList.remove("nav-open");
});
// Overlay closes the mobile drawer on tap (the ☰ toggle alone forced a two-step close).
$("overlay").addEventListener("click", () => document.body.classList.remove("nav-open"));
$("new-chat").addEventListener("click", () => {
  if (state.sending) { state.abort?.abort(); } // abort first: the in-flight stream must not leak into the fresh view
  generation++; // invalidate stale stream events arriving after abort (M8)
  state.session = "";
  resetTokenCount();
  resetTransient();
  eventsEl().innerHTML = "";
  stickBottom = true; // L10: reset so to-bottom button doesn't linger from previous turn
  document.title = "miniagent";
  showSessionID("");
  if (!isTouch) { $("prompt").focus(); } // L6: avoid forcing keyboard open on mobile
});

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

$("send").addEventListener("click", () => { if (state.sending) { state.abort?.abort(); return; } send(); });

async function send() {
  if (state.sending) return;
  const prompt = $("prompt").value.trim();
  const workdir = $("workdir").value.trim();
  if (!prompt) { inlineHint("请输入内容"); return; }
  if (!workdir) { inlineHint("请输入工作目录"); return; }
  const sel = $("model");
  const opt = sel.selectedOptions[0];
  const body = { prompt, workdir, session: state.session, provider: opt?.dataset.provider || "", model: opt?.dataset.model || "", thinking: opt?.dataset.thinking || "" };

  state.sending = true;
  state.lastPrompt = prompt;
  state.turnStartTs = 0;
  state.abort = new AbortController();
  const gen = generation; // M8: events from this stream are dropped if the view switched mid-turn
  const isCurrent = () => gen === generation;
  setComposer(false);
  startWait();
  appendUserPrompt(prompt);
  $("prompt").value = "";
  $("prompt").style.height = "auto";
  $("prompt").focus();
  saveWorkdir(workdir);
  scrollMo.observe(eventsEl(), { childList: true, subtree: true, characterData: true });

  let sawTerminal = false;
  try {
    const resp = await fetch("/api/turn", {
      method: "POST",
      headers: { "Content-Type": "application/json", ...authHeaders() },
      body: JSON.stringify(body),
      signal: state.abort.signal,
    });
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
          if (!isCurrent()) return; // M8: view switched (new-chat/openSession) — drop stale event
          if (ev.type === "result" || ev.type === "error") sawTerminal = true;
          stopWait();
          renderEvent(ev);
        } catch (e) { console.log("bad ndjson line", line, e); }
      }
    }
    if (buf.trim()) {
      try {
        const ev = JSON.parse(buf);
        if (!isCurrent()) return; // M8
        renderEvent(ev); if (ev.type === "result" || ev.type === "error") sawTerminal = true;
      } catch { /* trailing partial line */ }
    }
    // Stream ended without a terminal event = connection dropped mid-turn (e.g. server restart).
    if (!sawTerminal) {
      renderEvent({ type: "error", error: "连接中断：流意外结束，未收到 result/error 事件（会话已保存已执行部分，可点击重试续跑）", ts: Date.now() });
    }
  } catch (e) {
    if (e.name === "AbortError") {
      // M8: abort after a view switch is expected — don't paint the stop card into the new view
      if (!isCurrent()) return;
      renderEvent({ type: "error", error: "已停止（会话已保存已执行部分）", ts: Date.now() });
    } else {
      renderEvent({ type: "error", error: "请求失败：" + e.message, ts: Date.now() });
    }
  } finally {
    stopWait();
    if (isCurrent()) finishText(); // M8: leave a switched view's streaming blocks alone
    state.sending = false;
    state.abort = null;
    setComposer(true);
    scrollMo.disconnect();
    loadSessions();
  }
}

// Inline validation hint (replaces alert()): transient, next to the composer.
function inlineHint(msg) {
  const el = $("wait");
  el.hidden = false;
  el.textContent = msg;
  el.style.color = "var(--err)";
  setTimeout(() => { el.hidden = true; el.textContent = ""; el.style.color = ""; }, 2500);
}

// Inline confirm dialog (replaces native confirm()): resolves true on confirm, false otherwise.
// L8: avoids native alert/confirm which clash with inlineHint style and are not keyboard-navigable.
// okText labels the destructive action button (styled with --err), so the dialog is reusable.
function confirmInline(msg, okText) {
  const overlay = document.createElement("div");
  overlay.setAttribute("role", "dialog");
  overlay.setAttribute("aria-modal", "true");
  overlay.style.cssText = "position:fixed;inset:0;background:rgba(0,0,0,.5);z-index:1000;display:flex;align-items:center;justify-content:center;";
  const box = document.createElement("div");
  box.style.cssText = "background:var(--panel);color:var(--fg);border:1px solid var(--border);border-radius:8px;padding:20px;max-width:320px;width:90%;";
  const p = document.createElement("p");
  p.textContent = msg;
  p.style.cssText = "margin:0 0 16px;";
  const btnOk = document.createElement("button");
  btnOk.textContent = okText;
  btnOk.style.cssText = "padding:6px 16px;background:var(--err);color:#fff;border:none;border-radius:4px;cursor:pointer;";
  const btnCancel = document.createElement("button");
  btnCancel.textContent = "取消";
  btnCancel.style.cssText = "padding:6px 16px;background:transparent;color:var(--fg);border:1px solid var(--border);border-radius:4px;cursor:pointer;margin-left:8px;";
  const row = document.createElement("div");
  row.style.cssText = "text-align:right;";
  row.append(btnCancel, btnOk);
  box.append(p, row);
  overlay.append(box);
  document.body.append(overlay);
  btnCancel.focus();
  const focusables = [btnCancel, btnOk];
  // Focus trap: Tab cycles inside the box, Shift+Tab wraps backward, so keyboard users
  // can't escape into the page behind the modal while it's open (N2).
  overlay.addEventListener("keydown", (e) => {
    if (e.key !== "Tab") return;
    const last = focusables[focusables.length - 1], first = focusables[0];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  });
  return new Promise(resolve => {
    const close = (v) => { overlay.remove(); resolve(v); };
    btnOk.addEventListener("click", () => close(true));
    btnCancel.addEventListener("click", () => close(false));
    overlay.addEventListener("click", (e) => { if (e.target === overlay) close(false); });
    const onKey = (e) => {
      if (e.key === "Escape") close(false);
      // N2: Enter confirms ONLY when focus is on the destructive button. Focus starts on
      // Cancel; pressing Enter there must activate Cancel (the intuitive "my current button"
      // action) — not confirm a session deletion by accident. Default button behavior already
      // fires the focused button's click on Enter, so no special handling needed for that case.
      else if (e.key === "Enter" && document.activeElement === btnOk) close(true);
    };
    overlay.addEventListener("keydown", onKey);
  });
}

// Wait indicator for non-streaming configs: between send and the first event the UI would
// otherwise look dead (no deltas arrive until the terminal result).
let waitTimer = 0;
function startWait() {
  const el = $("wait");
  let dots = 0;
  el.hidden = false;
  waitTimer = setInterval(() => { dots = (dots + 1) % 4; el.textContent = `等待响应${".".repeat(dots)}`; }, 400);
}
function stopWait() {
  if (!waitTimer) return;
  clearInterval(waitTimer);
  waitTimer = 0;
  $("wait").hidden = true;
}

// Lock the composer while a turn runs; the send button becomes a stop button.
function setComposer(enabled) {
  $("prompt").disabled = !enabled;
  $("workdir").disabled = !enabled;
  $("model").disabled = !enabled;
  $("send").textContent = enabled ? "发送" : "停止";
  $("send").classList.toggle("danger", !enabled);
}

// ---- models / sessions ----

async function loadModels() {
  try {
    const r = await api("/api/models");
    const models = await r.json();
    const sel = $("model");
    sel.innerHTML = "";
    const saved = loadModel();
    let restored = false;
    for (const m of models) {
      const o = document.createElement("option");
      o.textContent = `${m.provider}/${m.model}`;
      o.dataset.provider = m.provider;
      o.dataset.model = m.model;
      o.dataset.thinking = m.thinking || "";
      sel.appendChild(o);
      if (saved === `${m.provider}/${m.model}`) { o.selected = true; restored = true; }
    }
    if (restored) setModelBadge(saved);
    sel.addEventListener("change", () => {
      const opt = sel.selectedOptions[0];
      const v = opt?.dataset.model ? `${opt.dataset.provider}/${opt.dataset.model}` : "";
      saveModel(v);
      setModelBadge(v);
    });
  } catch { /* dropdown stays default */ }
}

let sessionMeta = {}; // id → { workdir, model, ... } captured from the session list

// N5: the session list needs states beyond "empty" — a fresh install or after deleting every
// session otherwise shows a silent blank sidebar with no hint that the list loaded fine.
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
      b.className = "sess-item" + (s.id === state.session ? " active" : "");
      b.type = "button";
      const top = document.createElement("div");
      top.textContent = s.model || s.id;
      const sid = document.createElement("div");
      sid.className = "sid";
      sid.textContent = [s.id, s.workdir || "", s.created ? new Date(s.created).toLocaleString() : ""].filter(Boolean).join(" · ");
      b.appendChild(top); b.appendChild(sid);
      if (s.preview) {
        const pv = document.createElement("div");
        pv.className = "preview";
        pv.textContent = s.preview;
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
    // The old list was already cleared for "加载中…" — failing silently would strand that
    // placeholder forever. Show the error with a retry instead (third state, N5).
    hint.textContent = `加载失败：${e.message}（点击重试）`;
    hint.style.cursor = "pointer";
    hint.addEventListener("click", () => loadSessions());
  }
}

async function deleteSession(id) {
  if (await confirmInline(`删除会话 ${id}？此操作不可恢复。`, "删除")) {
    try {
      await api(`/api/sessions/${encodeURIComponent(id)}`, { method: "DELETE" });
      if (id === state.session) {
        state.session = "";
        resetTokenCount();
        resetTransient();
        stickBottom = true;
        eventsEl().innerHTML = "";
        document.title = "miniagent";
        showSessionID("");
      }
      loadSessions();
    } catch (e) { inlineHint("删除失败：" + e.message); }
  }
}

async function openSession(id) {
  if (state.sending) {
    // In-flight stream must not leak into the switched view: abort first (the server still
    // saves the executed part), then swap.
    state.abort?.abort();
  }
  state.session = id;
  generation++; // invalidate stale events from the aborted session (M8)
  const gen = generation; // a later new-chat/openSession bumps generation → drop this replay stream
  resetTokenCount();
  resetTransient();
  document.title = `miniagent · ${id}`;
  document.body.classList.remove("nav-open");
  eventsEl().innerHTML = "";
  stickBottom = true;
  if (sessionMeta[id]?.workdir) { $("workdir").value = sessionMeta[id].workdir; saveWorkdir(sessionMeta[id].workdir); }
  // N14: sessionMeta may be stale (refresh → click before loadSessions finishes). The replay
  // stream's first event (session) carries workdir — fill the input if still empty.
  let workdirFilled = !!sessionMeta[id]?.workdir;
  try {
    const r = await api(`/api/sessions/${encodeURIComponent(id)}`);
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
          if (gen !== generation) return; // M8: view switched mid-replay — stop painting
          // N14: if the session event carries workdir and the input is still empty, fill it.
          // The empty-input guard keeps a user-typed workdir from being overwritten by the
          // session's own — an explicit value always wins over an implicit one.
          if (!workdirFilled && ev.type === "session" && ev.workdir && !$("workdir").value.trim()) {
            $("workdir").value = ev.workdir;
            saveWorkdir(ev.workdir);
            workdirFilled = true;
          }
          renderEvent(ev);
        } catch { /* skip bad lines */ }
      }
    }
    if (buf.trim()) { try { if (gen === generation) renderEvent(JSON.parse(buf)); } catch { /* trailing partial line */ } }
    if (gen !== generation) return; // a newer view owns the DOM now — don't touch its title/id/focus
    showSessionID(state.session);
    finishText();
  } catch (e) { console.log("replay failed", e); }
  if (!isTouch && gen === generation) { $("prompt").focus(); } // L6 + M8
}

boot();
