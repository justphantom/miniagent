"use strict";

// ---- event rendering: NDJSON events → DOM, markdown body, collapse, copy button ----

import { mdRender, esc } from "./md.js";
import { state, fmtTime, showSessionID, setModelBadge } from "./store.js";

const LONG_TEXT_LINES = 24; // assistant/result text beyond this collapses with a fade + expand toggle

let curText = null, curReasoning = null;
let toolNodes = new Map(); // callID → tool <details> node, for exact tool_result pairing

function eventsEl() { return document.getElementById("events"); }

// evDiv: body "" = streaming span placeholder (caller appends), string = final markdown body.
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

export function appendUserPrompt(text) {
  const d = evDiv("user", "user", Date.now());
  d.appendChild(document.createTextNode(text)); // user input stays plain text (no markdown)
  eventsEl().appendChild(d);
}

export function appendDelta(kind, text, ts) {
  let d = kind === "text" ? curText : curReasoning;
  if (!d) {
    d = evDiv(kind === "text" ? "text" : "reasoning", kind === "text" ? "assistant" : "assistant · thinking", ts);
    if (kind === "reasoning") d.style.opacity = "0.75";
    d._md = ""; // raw markdown buffer, rendered at finishText
    eventsEl().appendChild(d);
    const span = document.createElement("span");
    d.appendChild(span);
    if (kind === "text") curText = d; else curReasoning = d;
    d._span = span;
  }
  d._md += text;
  d._span.textContent += text; // streaming: plain text, zero flicker
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

export function appendToolUse(ev) {
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
  eventsEl().appendChild(d);
  if (ev.call_id) toolNodes.set(ev.call_id, d);
  return d;
}

// tool_result carries call_id: pair it with the exact tool_use node. Fallback to the last
// tool block only when the id is unknown (replay of old sessions predating call_id).
export function appendToolResult(ev, toolNode) {
  const target = toolNode || (ev.call_id && toolNodes.get(ev.call_id)) || eventsEl().querySelector("details.ev.tool:last-of-type");
  if (!target) return;
  const pre = document.createElement("pre");
  pre.className = "out" + (ev.is_error ? " err" : "");
  pre.textContent = ev.output || "";
  target.appendChild(pre);
}

export function finishText() {
  for (const d of [curText, curReasoning]) {
    if (!d) continue;
    if (d._span) d._span.remove();
    const md = document.createElement("div");
    md.className = "md";
    md.innerHTML = mdRender(d._md || "");
    d.appendChild(md);
    addCopyButtons(md);
    makeCollapsible(d, d._md || "");
    delete d._md; delete d._span;
  }
  curText = curReasoning = null;
}

// resetTransient drops per-view state (callID map); openSession/new-chat call this after
// clearing the DOM so stale tool nodes from the previous view are never paired against.
export function resetTransient() {
  toolNodes = new Map();
}

export function renderEvent(ev) {
  if (state.turnStartTs === 0 && ev.ts) state.turnStartTs = ev.ts;
  switch (ev.type) {
    case "session":
      state.session = ev.id;
      document.title = `miniagent · ${ev.id}`;
      showSessionID(ev.id);
      break;
    case "text_delta": appendDelta("text", ev.text, ev.ts); break;
    case "reasoning_delta": appendDelta("reasoning", ev.text, ev.ts); break;
    case "tool_use": finishText(); appendToolUse(ev); break;
    case "tool_result": finishText(); appendToolResult(ev, null); break;
    case "result": {
      finishText();
      if (ev.model) setModelBadge(ev.model);
      state.sessionInTokens += ev.input_tokens || 0;
      state.sessionOutTokens += ev.output_tokens || 0;
      showSessionID(state.session); // refresh token counter
      const d = evDiv("result", "result", ev.ts);
      const md = document.createElement("div");
      md.className = "md";
      md.innerHTML = mdRender(ev.text || "(no text)");
      d.appendChild(md);
      addCopyButtons(md);
      makeCollapsible(d, ev.text || "");
      const u = document.createElement("div");
      u.className = "usage";
      const elapsed = state.turnStartTs && ev.ts ? ` · ${((ev.ts - state.turnStartTs) / 1000).toFixed(1)}s` : "";
      u.textContent = `steps=${ev.steps} in=${ev.input_tokens} out=${ev.output_tokens}${ev.compacted ? " compacted" : ""}${elapsed}`;
      d.appendChild(u);
      eventsEl().appendChild(d);
      break;
    }
    case "error": {
      finishText();
      const d = evDiv("error", "error", ev.ts || Date.now());
      d.appendChild(document.createTextNode(ev.error || ev.message || "unknown error"));
      if (state.lastPrompt) {
        const retry = document.createElement("button");
        retry.type = "button";
        retry.className = "ghost retry-btn";
        retry.textContent = "重试";
        retry.addEventListener("click", () => { document.getElementById("prompt").value = state.lastPrompt; document.getElementById("send").click(); });
        d.appendChild(retry);
      }
      eventsEl().appendChild(d);
      break;
    }
  }
}
