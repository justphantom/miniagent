"use strict";

// ---- event rendering: NDJSON events → per-view DOM, markdown body, collapse, copy ----
// Every render function takes the target view: views are independent (multi-session UI),
// each holding its own in-flight delta nodes, tool-node map and token counters.

import { mdRender } from "./md.js";
import { fmtTime } from "./store.js";
import { activeView } from "./views.js";

const LONG_TEXT_LINES = 24; // assistant/result text beyond this collapses with a fade + expand toggle

// Streaming markdown: reasoning deltas stream for minutes and the plain-text phase showed raw
// md syntax (`**bold**`, fences) the whole time. Reparse at a fixed cadence instead of per
// delta (O(deltas) reparses caused the original one-shot-render decision), and cap the live
// buffer — pathological streams wait for finishText instead of re-parsing megabytes per tick.
// Hidden views skip repaint (their buffers finalize at finishText).
const STREAM_RENDER_MS = 300;
const MAX_STREAM_MD_CHARS = 64 * 1024;

const dirtyViews = new Set(); // views with unrendered streaming buffers
let streamTimer = 0;

// evDiv: body "" = streaming container (caller appends), string = final markdown body.
function evDiv(cls, tag, ts, body = "") {
  const d = document.createElement("div");
  d.className = "ev " + cls;
  if (tag) {
    const t = document.createElement("div");
    t.className = "tag";
    const label = document.createElement("span");
    label.textContent = tag;
    t.appendChild(label);
    const time = document.createElement("span");
    time.className = "time";
    time.textContent = fmtTime(ts);
    t.appendChild(time);
    d.appendChild(t);
  }
  if (body) {
    const md = document.createElement("div");
    md.className = "md";
    md.innerHTML = mdRender(body);
    d.appendChild(md);
    addCopyButtons(md);
  }
  return d;
}

export function appendUserPrompt(view, text) {
  const d = evDiv("user", "user", Date.now());
  d.appendChild(document.createTextNode(text)); // user input stays plain text (no markdown)
  view.dom.appendChild(d);
}

export function appendDelta(view, kind, text, ts) {
  let d = kind === "text" ? view.curText : view.curReasoning;
  if (!d) {
    d = evDiv(kind === "text" ? "text" : "reasoning", kind === "text" ? "assistant" : "assistant · thinking", ts);
    if (kind === "reasoning") d.style.opacity = "0.75";
    d._md = ""; // raw markdown buffer, kept live by paintStreaming, finalized at finishText
    const md = document.createElement("div");
    md.className = "md";
    d.appendChild(md);
    d._mdEl = md;
    view.dom.appendChild(d);
    if (kind === "text") view.curText = d; else view.curReasoning = d;
  }
  d._md += text;
  dirtyViews.add(view);
  if (!streamTimer) streamTimer = setInterval(paintStreaming, STREAM_RENDER_MS);
  if (!d._painted) { d._mdEl.innerHTML = mdRender(d._md); d._painted = true; } // first chunk paints immediately
}

// paintStreaming re-renders every visible view's in-flight buffers on the shared timer. Copy
// buttons and collapse still bind once at finishText (re-binding per tick would duplicate
// listeners on every <pre>).
function paintStreaming() {
  for (const view of dirtyViews) {
    if (view.dom.style.display === "none") continue; // hidden view: finishText finalizes
    for (const d of [view.curText, view.curReasoning]) {
      if (d?._mdEl && d._md.length <= MAX_STREAM_MD_CHARS) d._mdEl.innerHTML = mdRender(d._md);
    }
  }
}

// ---- collapsible blocks with one-click copy for code ----

function addCopyButtons(root) {
  for (const pre of root.querySelectorAll("pre")) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "copy-btn";
    btn.textContent = "复制";
    btn.addEventListener("click", () => {
      navigator.clipboard.writeText(pre.querySelector("code")?.textContent ?? pre.textContent).then(() => {
        btn.textContent = "已复制";
        setTimeout(() => { btn.textContent = "复制"; }, 1500);
      });
    });
    pre.appendChild(btn);
  }
}

function makeCollapsible(d, text) {
  const lines = text.split("\n").length;
  if (lines <= LONG_TEXT_LINES) return;
  d.classList.add("collapsed");
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "expand-btn";
  btn.textContent = `展开全部（${lines} 行）`;
  btn.addEventListener("click", () => {
    const nowCollapsed = d.classList.toggle("collapsed");
    btn.textContent = nowCollapsed ? `展开全部（${lines} 行）` : "收起";
  });
  d.appendChild(btn);
}

export function appendToolUse(view, ev) {
  const d = document.createElement("details");
  d.className = "ev tool";
  const s = document.createElement("summary");
  const name = document.createElement("span");
  name.textContent = `🔧 ${ev.name}`;
  const time = document.createElement("span");
  time.className = "time";
  time.textContent = fmtTime(ev.ts);
  s.appendChild(name); s.appendChild(time);
  d.appendChild(s);
  const pre = document.createElement("pre");
  pre.textContent = ev.input || "";
  d.appendChild(pre);
  view.dom.appendChild(d);
  if (ev.call_id) view.toolNodes.set(ev.call_id, d);
  return d;
}

// tool_result carries call_id: pair it with the exact tool_use node. Fallback to the last
// tool block only when the id is unknown (replay of old sessions predating call_id).
export function appendToolResult(view, ev) {
  const target = (ev.call_id && view.toolNodes.get(ev.call_id)) || view.dom.querySelector("details.ev.tool:last-of-type");
  if (!target) return;
  const pre = document.createElement("pre");
  pre.className = "out" + (ev.is_error ? " err" : "");
  pre.textContent = ev.output || "";
  target.appendChild(pre);
}

export function finishText(view) {
  for (const d of [view.curText, view.curReasoning]) {
    if (!d) continue;
    if (d._mdEl) {
      d._mdEl.innerHTML = mdRender(d._md || ""); // final full-fidelity render
      addCopyButtons(d._mdEl);
    }
    makeCollapsible(d, d._md || "");
    delete d._md; delete d._mdEl; delete d._painted;
  }
  view.curText = view.curReasoning = null;
  dirtyViews.delete(view);
  if (dirtyViews.size === 0 && streamTimer) { clearInterval(streamTimer); streamTimer = 0; }
}

// resetTransient drops a view's per-view state — openSession/rebuild call this after clearing
// the DOM so stale tool nodes are never paired against.
export function resetTransient(view) {
  view.toolNodes = new Map();
}

export function renderEvent(view, ev) {
  if (view.turnStartTs === 0 && ev.ts) view.turnStartTs = ev.ts;
  switch (ev.type) {
    case "session":
      break; // id/workdir are consumed by the caller (rekey/header) before render
    case "text_delta": appendDelta(view, "text", ev.text, ev.ts); break;
    case "reasoning_delta": appendDelta(view, "reasoning", ev.text, ev.ts); break;
    case "tool_use": finishText(view); appendToolUse(view, ev); break;
    case "tool_result": finishText(view); appendToolResult(view, ev); break;
    case "result": {
      finishText(view);
      view.tokens.in += ev.input_tokens || 0;
      view.tokens.out += ev.output_tokens || 0;
      const d = evDiv("result", "result", ev.ts);
      const md = document.createElement("div");
      md.className = "md";
      md.innerHTML = mdRender(ev.text || "(no text)");
      d.appendChild(md);
      addCopyButtons(md);
      makeCollapsible(d, ev.text || "");
      const u = document.createElement("div");
      u.className = "usage";
      const elapsed = view.turnStartTs && ev.ts ? ` · ${((ev.ts - view.turnStartTs) / 1000).toFixed(1)}s` : "";
      u.textContent = `steps=${ev.steps} in=${ev.input_tokens} out=${ev.output_tokens}${ev.compacted ? " compacted" : ""}${elapsed}`;
      d.appendChild(u);
      view.dom.appendChild(d);
      view.turnStartTs = 0; // N13: replay streams several results — reset so the next turn's elapsed starts fresh
      break;
    }
    case "error": {
      finishText(view);
      const d = evDiv("error", "error", ev.ts || Date.now());
      d.appendChild(document.createTextNode(ev.error || ev.message || "unknown error"));
      if (view.lastPrompt) {
        const retry = document.createElement("button");
        retry.type = "button";
        retry.className = "ghost retry-btn";
        retry.textContent = "重试";
        retry.addEventListener("click", () => {
          document.getElementById("prompt").value = view.lastPrompt;
          if (activeView() === view) document.getElementById("send").click(); // auto-send only when still on this view
        });
        d.appendChild(retry);
      }
      view.dom.appendChild(d);
      break;
    }
  }
}
