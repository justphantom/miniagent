"use strict";

// sessions.js — session list rendering, deletion, replay-loading, and empty-state hint.
// openSession (view switching) stays in app.js: it calls attachSpectator (send.js), which
// would otherwise create a sessions→send→sessions import cycle.

import { api, saveWorkdir } from "./store.js";
import { $, inlineHint, updateHeader, observeScroll, disconnectScroll, confirmInline } from "./ui.js";
import { renderEvent, finishText } from "./events.js";
import { activate, createView, byID, dropView, activeView } from "./views.js";
import { refreshPanel } from "./trajectory.js";

let sessionMeta = {}; // id → { workdir, model, running, ... } captured from the session list

// N5: the session list carries three states (loading/empty/error-with-retry).
export async function loadSessions() {
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
    for (const s of list) sessionMeta[s.id] = s;
    // Group sessions by workdir (project tree): one .tree-group per directory.
    const groups = new Map();
    for (const s of list) {
      const k = s.workdir || "";
      if (!groups.has(k)) groups.set(k, []);
      groups.get(k).push(s);
    }
    for (const [wd, sessions] of groups) {
      const g = document.createElement("div");
      g.className = "tree-group";
      const gt = document.createElement("div");
      gt.className = "tree-group-title";
      gt.textContent = wd || "（无工作目录）";
      gt.title = wd;
      g.appendChild(gt);
      for (const s of sessions) {
        const b = document.createElement("button");
        b.className = "sess-item" + (s.id === activeView()?.id ? " active" : "") + (s.running ? " running" : "");
        b.type = "button";
        const top = document.createElement("div");
        top.className = "sess-title";
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
        b.addEventListener("click", () => {
          // Delegate open to app.js via a DOM event (avoids a sessions→app import cycle).
          document.dispatchEvent(new CustomEvent("open-session", { detail: s.id }));
        });
        g.appendChild(b);
      }
      box.appendChild(g);
    }
  } catch (e) {
    hint.textContent = `加载失败：${e.message}（点击重试）`;
    hint.style.cursor = "pointer";
    hint.addEventListener("click", () => loadSessions());
  }
}

// getMeta exposes the session list metadata (workdir/running) to app.js for openSession.
export function getMeta(id) { return sessionMeta[id]; }

export async function deleteSession(id) {
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

// ensureEmptyState shows the empty-state hint when the view has no content yet.
export function ensureEmptyState(v) {
  if (v.dom.children.length === 0 && !v.sending) {
    const hint = document.createElement("div");
    hint.className = "ev empty-state muted";
    hint.textContent = "暂无对话，输入任务开始";
    v.dom.appendChild(hint);
  }
}

// loadReplay streams the persisted history into the view (tail-capped server-side). The
// session event fills workdir when the input is still empty (N14: explicit input wins).
export async function loadReplay(v) {
  const gen = ++v.gen;
  v.usage.budget = 0;
  v.usage.steps.length = 0;
  v.trajectory.order.length = 0;
  v.trajectory.steps.clear();
  v.curStep = 0;
  v.metrics = { rounds: 0, steps: 0, llmMs: 0, toolMs: 0 };
  v.workdir = sessionMeta[v.id]?.workdir || "";
  if (v.workdir && !$("workdir").value.trim()) { $("workdir").value = v.workdir; saveWorkdir(v.workdir); }
  let workdirFilled = !!v.workdir;
  observeScroll();
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
          if (v.gen !== gen) { disconnectScroll(); return; } // superseded: view rebuilt/stream took over
          if (!workdirFilled && ev.type === "session" && ev.workdir && !$("workdir").value.trim()) {
            v.workdir = ev.workdir;
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
  disconnectScroll();
}
