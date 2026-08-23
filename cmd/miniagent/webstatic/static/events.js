"use strict";

// ---- event rendering: NDJSON events → per-view DOM, markdown body, collapse, copy ----
// Every render function takes the target view: views are independent (multi-session UI),
// each holding its own in-flight delta nodes, tool-node map and token counters.

import { mdRender } from "./md.js";
import { fmtTime, getBudget } from "./store.js";
import { activeView } from "./views.js";
import { renderUsageBar, renderStepUsageList } from "./usage.js";
import { refreshPanel } from "./trajectory.js";

const LONG_TEXT_LINES = 24; // assistant/result text beyond this collapses with a fade + expand toggle

// Step timeline icons (V3): each event card gets a leading icon by type/tool name.
const STEP_ICONS = {
  think: "💭", bash: ">_", read: "📄", write: "✏️", edit: "📝",
  search: "🔍", web: "🌐", result: "✅", error: "❌", user: "👤",
  assistant: "🤖", default: "◈",
};
function getIcon(name) {
  const key = (name || "").toLowerCase().split(/[_\s]/)[0];
  return STEP_ICONS[key] || STEP_ICONS.default;
}

// Streaming markdown: reasoning deltas stream for minutes and the plain-text phase showed raw
// md syntax (`**bold**`, fences) the whole time. Reparse at a fixed cadence instead of per
// delta (O(deltas) reparses caused the original one-shot-render decision), and cap the live
// buffer — pathological streams wait for finishText instead of re-parsing megabytes per tick.
// Hidden views skip repaint (their buffers finalize at finishText).
const STREAM_RENDER_MS = 300;
const MAX_STREAM_MD_CHARS = 64 * 1024;

const dirtyViews = new Set(); // views with unrendered streaming buffers
let streamTimer = 0;

// evDiv: body "" = streaming container (caller appends into .ev-body), string = final markdown body.
function evDiv(cls, tag, ts, body = "", icon = "") {
  const d = document.createElement("div");
  d.className = "ev " + cls;
  const ic = document.createElement("div");
  ic.className = "ev-step-icon";
  ic.textContent = icon || STEP_ICONS.default;
  d.appendChild(ic);
  const wrap = document.createElement("div");
  wrap.className = "ev-body";
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
    wrap.appendChild(t);
  }
  if (body) {
    const md = document.createElement("div");
    md.className = "md";
    md.innerHTML = mdRender(body);
    wrap.appendChild(md);
    addCopyButtons(md);
  }
  d.appendChild(wrap);
  return d;
}

export function appendUserPrompt(view, text) {
  // remove empty state if present
  view.dom.querySelector(".empty-state")?.remove();
  const d = evDiv("user", "user", Date.now(), "", STEP_ICONS.user);
  d.querySelector(".ev-body").appendChild(document.createTextNode(text)); // user input stays plain text (no markdown)
  view.dom.appendChild(d);
}

export function appendDelta(view, kind, text, ts, step) {
  if (step) view.curStep = step;
  let d = kind === "text" ? view.curText : view.curReasoning;
  if (!d) {
    d = evDiv(kind === "text" ? "text" : "reasoning", kind === "text" ? "assistant" : "assistant · thinking", ts, "",
      kind === "text" ? STEP_ICONS.assistant : STEP_ICONS.think);
    if (kind === "reasoning") d.style.opacity = "0.75";
    d._md = ""; // raw markdown buffer, kept live by paintStreaming, finalized at finishText
    const md = document.createElement("div");
    md.className = "md";
    d.querySelector(".ev-body").appendChild(md);
    d._mdEl = md;
    view.dom.appendChild(d);
    if (kind === "text") view.curText = d; else view.curReasoning = d;
  }
  d._md += text;
  dirtyViews.add(view);
  if (!streamTimer) streamTimer = setInterval(paintStreaming, STREAM_RENDER_MS);
  if (step) d.dataset.step = step;
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
  (d.querySelector(".ev-body") || d).appendChild(btn);
}

export function appendToolUse(view, ev) {
  const d = document.createElement("details");
  d.className = "ev tool";
  if (ev.step) d.dataset.step = ev.step;
  const ic = document.createElement("div");
  ic.className = "ev-step-icon";
  ic.textContent = getIcon(ev.name);
  const wrap = document.createElement("div");
  wrap.className = "ev-body";
  const s = document.createElement("summary");
  const name = document.createElement("span");
  name.textContent = ev.name;
  const time = document.createElement("span");
  time.className = "time";
  time.textContent = fmtTime(ev.ts);
  s.appendChild(name); s.appendChild(time);
  wrap.appendChild(s);
  const pre = document.createElement("pre");
  pre.textContent = ev.input || "";
  wrap.appendChild(pre);
  d.append(ic, wrap);
  view.dom.appendChild(d);
  if (ev.call_id) view.toolNodes.set(ev.call_id, d);
  // trajectory capture
  const step = ev.step ?? view.curStep;
  if (step) {
    captureToolUse(view, step, ev);
  }
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
  (target.querySelector(".ev-body") || target).appendChild(pre);
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

// ---- trajectory capture (mirrors tool events into view.trajectory without touching the message DOM) ----

function captureToolUse(view, step, ev) {
  if (!view.trajectory.order.includes(step)) view.trajectory.order.push(step);
  let entry = view.trajectory.steps.get(step);
  if (!entry) {
    entry = { step, ts: ev.ts || Date.now(), in: 0, out: 0, tools: [] };
    view.trajectory.steps.set(step, entry);
  }
  entry.tools.push({ name: ev.name, callID: ev.call_id, input: ev.input, output: "", isError: false, ts: ev.ts });
}

function captureToolResult(view, ev) {
  for (const entry of view.trajectory.steps.values()) {
    for (const t of entry.tools) {
      if (t.callID === ev.call_id) {
        t.output = ev.output || "";
        t.isError = !!ev.is_error;
        return;
      }
    }
  }
}

export function renderEvent(view, ev) {
  if (view.turnStartTs === 0 && ev.ts) view.turnStartTs = ev.ts;
  // remove empty state on first real event
  if (ev.type !== "session") view.dom.querySelector(".empty-state")?.remove();
  switch (ev.type) {
    case "session":
      break; // id/workdir are consumed by the caller (rekey/header) before render
    case "text_delta": appendDelta(view, "text", ev.text, ev.ts, ev.step); break;
    case "reasoning_delta": appendDelta(view, "reasoning", ev.text, ev.ts, ev.step); break;
    case "tool_use": finishText(view); appendToolUse(view, ev); break;
    case "tool_result": finishText(view); appendToolResult(view, ev); captureToolResult(view, ev); break;
    case "result": {
      finishText(view);
      view.tokens.in += ev.input_tokens || 0;
      view.tokens.out += ev.output_tokens || 0;
      const d = evDiv("result", "result", ev.ts, "", STEP_ICONS.result);
      const body = d.querySelector(".ev-body");
      const md = document.createElement("div");
      md.className = "md";
      md.innerHTML = mdRender(ev.text || "(no text)");
      body.appendChild(md);
      addCopyButtons(md);
      makeCollapsible(d, ev.text || "");
      const u = document.createElement("div");
      u.className = "usage";
      const elapsed = view.turnStartTs && ev.ts ? ` · ${((ev.ts - view.turnStartTs) / 1000).toFixed(1)}s` : "";
      u.textContent = `steps=${ev.steps} in=${ev.input_tokens} out=${ev.output_tokens}${ev.compacted ? " compacted" : ""}${elapsed}`;
      body.appendChild(u);
      // usage bar + step details
      const budget = view.usage.budget || getBudget();
      renderUsageBar(body, { in: ev.input_tokens || 0, out: ev.output_tokens || 0, budget });
      renderStepUsageList(body, view.usage.steps);
      view.usage.steps.length = 0;
      view.dom.appendChild(d);
      view.turnStartTs = 0; // N13: replay streams several results — reset so the next turn's elapsed starts fresh
      view.curStep = 0;
      break;
    }
    case "step_usage": {
      view.usage.steps.push({ step: ev.step, in: ev.input_tokens || 0, out: ev.output_tokens || 0, toolCalls: ev.tool_calls || 0 });
      // sync trajectory step usage
      const entry = view.trajectory.steps.get(ev.step);
      if (entry) { entry.in = ev.input_tokens || 0; entry.out = ev.output_tokens || 0; }
      // F5: live-refresh the trajectory panel when it's open (per-step, not per-tool, to avoid flicker)
      if (!document.getElementById("trajectory-panel")?.hidden) refreshPanel();
      break;
    }
    case "error": {
      finishText(view);
      const d = evDiv("error", "error", ev.ts || Date.now(), "", STEP_ICONS.error);
      const body = d.querySelector(".ev-body");
      body.appendChild(document.createTextNode(ev.error || ev.message || "unknown error"));
      if (view.lastPrompt) {
        const retry = document.createElement("button");
        retry.type = "button";
        retry.className = "ghost retry-btn";
        retry.textContent = "重试";
        retry.addEventListener("click", () => {
          document.getElementById("prompt").value = view.lastPrompt;
          if (activeView() === view) document.getElementById("send").click(); // auto-send only when still on this view
        });
        body.appendChild(retry);
      }
      view.dom.appendChild(d);
      break;
    }
  }
}
