"use strict";

// miniagent -serve WebUI: auth gate, session list, NDJSON streaming composer.
// ES modules, no build step; assets are embedded by webstatic (go:embed).

import { state, setKey, api, authHeaders, showSessionID, resetTokenCount, saveWorkdir, loadWorkdir, saveTheme, loadTheme, setVersion } from "./store.js";
import { appendUserPrompt, renderEvent, finishText } from "./events.js";

const $ = (id) => document.getElementById(id);
state.lastPrompt = ""; // last sent prompt, used by the error retry button

// ---- auth gate ----

async function boot() {
  const theme = loadTheme();
  if (theme) document.documentElement.dataset.theme = theme;
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
$("theme-btn").addEventListener("click", () => {
  const next = document.documentElement.dataset.theme === "light" ? "" : "light";
  document.documentElement.dataset.theme = next;
  saveTheme(next);
});

// ---- auto-scroll: follow only when near bottom, otherwise show a jump button ----

let stickBottom = true;
const NEAR_BOTTOM = 80;

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
$("new-chat").addEventListener("click", () => {
  state.session = "";
  resetTokenCount();
  eventsEl().innerHTML = "";
  document.title = "miniagent";
  showSessionID("");
  $("prompt").focus();
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
  if (!prompt) { alert("请输入内容"); return; }
  if (!workdir) { alert("请输入工作目录"); return; }
  const sel = $("model");
  const opt = sel.selectedOptions[0];
  const body = { prompt, workdir, session: state.session, provider: opt?.dataset.provider || "", model: opt?.dataset.model || "", thinking: opt?.dataset.thinking || "" };

  state.sending = true;
  state.lastPrompt = prompt;
  state.turnStartTs = 0;
  state.abort = new AbortController();
  setComposer(false);
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
          if (ev.type === "result" || ev.type === "error") sawTerminal = true;
          renderEvent(ev);
        } catch (e) { console.log("bad ndjson line", line, e); }
      }
    }
    if (buf.trim()) {
      try { const ev = JSON.parse(buf); renderEvent(ev); if (ev.type === "result" || ev.type === "error") sawTerminal = true; } catch { /* trailing partial line */ }
    }
    // Stream ended without a terminal event = connection dropped mid-turn (e.g. server restart).
    if (!sawTerminal) {
      renderEvent({ type: "error", error: "连接中断：流意外结束，未收到 result/error 事件（会话已保存已执行部分，可点击重试续跑）", ts: Date.now() });
    }
  } catch (e) {
    if (e.name === "AbortError") {
      renderEvent({ type: "error", error: "已停止（会话已保存已执行部分）", ts: Date.now() });
    } else {
      renderEvent({ type: "error", error: "请求失败：" + e.message, ts: Date.now() });
    }
  } finally {
    finishText();
    state.sending = false;
    state.abort = null;
    setComposer(true);
    scrollMo.disconnect();
    loadSessions();
  }
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
    for (const m of models) {
      const o = document.createElement("option");
      o.textContent = `${m.provider}/${m.model}`;
      o.dataset.provider = m.provider;
      o.dataset.model = m.model;
      o.dataset.thinking = "";
      sel.appendChild(o);
    }
  } catch { /* dropdown stays default */ }
}

let sessionMeta = {}; // id → { workdir, model, ... } captured from the session list

async function loadSessions() {
  try {
    const r = await api("/api/sessions");
    const list = await r.json();
    if (!Array.isArray(list)) return;
    const box = $("session-list");
    box.innerHTML = "";
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
      const del = document.createElement("span");
      del.className = "sess-del";
      del.textContent = "✕";
      del.title = "删除会话";
      del.addEventListener("click", (e) => { e.stopPropagation(); deleteSession(s.id); });
      b.appendChild(del);
      b.addEventListener("click", () => openSession(s.id));
      box.appendChild(b);
    }
  } catch { /* keep old list */ }
}

async function deleteSession(id) {
  if (!confirm(`删除会话 ${id}？此操作不可恢复。`)) return;
  try {
    await api(`/api/sessions/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (id === state.session) {
      state.session = "";
      resetTokenCount();
      eventsEl().innerHTML = "";
      document.title = "miniagent";
      showSessionID("");
    }
    loadSessions();
  } catch (e) { alert("删除失败：" + e.message); }
}

async function openSession(id) {
  state.session = id;
  resetTokenCount();
  document.title = `miniagent · ${id}`;
  document.body.classList.remove("nav-open");
  eventsEl().innerHTML = "";
  stickBottom = true;
  if (sessionMeta[id]?.workdir) { $("workdir").value = sessionMeta[id].workdir; saveWorkdir(sessionMeta[id].workdir); }
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
          if (ev.type === "result") { // accumulate historical usage for the token counter
            state.sessionInTokens += ev.input_tokens || 0;
            state.sessionOutTokens += ev.output_tokens || 0;
          }
          renderEvent(ev);
        } catch { /* skip bad lines */ }
      }
    }
    if (buf.trim()) { try { renderEvent(JSON.parse(buf)); } catch { /* trailing partial line */ } }
    showSessionID(state.session);
    finishText();
  } catch (e) { console.log("replay failed", e); }
  $("prompt").focus();
}

boot();
