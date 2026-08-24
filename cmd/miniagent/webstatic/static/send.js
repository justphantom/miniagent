"use strict";

// send.js — the live turn stream driver: send(), stopTurn(), healAfterCut(), attachSpectator().
// Shared turn-liveness registry (runningKnown) lives here; consumed by app.js lifecycle.

import { api, authHeaders, saveWorkdir } from "./store.js";
import { rekey, activeView, eventsViewport } from "./views.js";
import { appendUserPrompt, renderEvent, finishText, resetTransient } from "./events.js";
import { attachLive } from "./live.js";
import { $, updateHeader, updateComposer, inlineHint, startWait, stopWait, observeScroll, disconnectScroll } from "./ui.js";
import { loadSessions, loadReplay } from "./sessions.js";

// session ids with a turn in flight (lifecycle feed; drives heal/spectator decisions).
export const runningKnown = new Set();

// send POSTs a turn and paints the NDJSON stream into the active view. The send button
// doubles as stop while a turn runs (wired in app.js); stopping goes through the stop API and
// the local stream stays open to receive the saved partial result (D1).
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
  observeScroll();

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
          if (ev.type === "session" && ev.workdir) v.workdir = ev.workdir;
          if (ev.type === "result" || ev.type === "error" || ev.type === "stop") sawTerminal = true;
          if (ev.type === "stream_cut") { sawTerminal = true; healAfterCut(v, "流被服务端中断，正在重建…"); return; }
          stopWait();
          renderEvent(v, ev);
          updateHeader();
        } catch (e) { console.log("bad ndjson line", line, e); }
      }
    }
    if (!sawTerminal) {
      healAfterCut(v, "流被中断，正在重建…");
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
    disconnectScroll();
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

// healAfterCut rebuilds the view after the server cut the NDJSON stream (subscriber lag —
// the bus closes slow subscribers and signals stream_cut; an EOF without a terminal event
// means the same). The turn keeps running server-side (D1): /live replays from event zero
// when still running, the persisted jsonl replays when it has finished. Falls back to the
// honest "连接中断" card only when both rebuild paths fail.
async function healAfterCut(v, hint) {
  if (!v.id) { // session event never arrived — nothing to rebuild from
    renderEvent(v, { type: "error", error: "连接中断：流意外结束（会话已保存已执行部分，可点击重试续跑）", ts: Date.now() });
    return;
  }
  v.gen++;            // supersede any in-flight stream/stale renderers
  v.dom.innerHTML = "";
  v.curText = null; v.curReasoning = null; v.toolNodes.clear();
  v.tokens = { in: 0, out: 0 };
  v.usage = { budget: 0, steps: [] };
  v.trajectory = { order: [], steps: new Map() };
  v.metrics = { rounds: 0, steps: 0, llmMs: 0, toolMs: 0 };
  v.curStep = 0;
  resetTransient(v);
  finishText(v);
  const note = document.createElement("div");
  note.className = "ev spectator muted";
  note.textContent = hint || "流被中断，正在重建…";
  v.dom.appendChild(note);
  try {
    if (runningKnown.has(v.id)) {
      attachSpectator(v, false); // /live replays from event zero and follows to live_end
      note.remove();
      return;
    }
    await loadReplay(v); // turn already finished: the jsonl has the full history
    if (!v.dom.querySelector(".ev.result, .ev.error, .ev.stopped")) throw new Error("empty replay");
    note.remove();
  } catch {
    note.remove();
    renderEvent(v, { type: "error", error: "连接中断：流意外结束（会话已保存已执行部分，可点击重试续跑）", ts: Date.now() });
  }
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

export { send, stopTurn, healAfterCut, attachSpectator };
