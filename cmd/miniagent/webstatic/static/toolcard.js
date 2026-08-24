"use strict";

// toolcard.js — Tool card DOM helpers: collapsible blocks, copy buttons, tool preview, icons.
// Imported by events.js for event rendering. No dependency on events.js (no circular dep).

import { fmtTime } from "./store.js";

const LONG_TEXT_LINES = 24; // assistant/result text beyond this collapses with a fade + expand toggle
const TOOL_PREVIEW_CHARS = 90; // tool card input/output preview caps (expand button shows full)
const TOOL_PREVIEW_LINES = 2;

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

// toolArg picks the headline argument for a tool summary (e.g. "edit main.go"). File tools
// show the basename (full path goes into title); shell shows the command, web the url,
// search tools the pattern. Unknown tools fall back to the first string field.
function toolArg(name, input) {
  let v = {};
  try { v = JSON.parse(input || "{}"); } catch { return ""; }
  const key = ["path", "command", "url", "pattern", "query"].find((k) => typeof v[k] === "string" && v[k]) || Object.keys(v).find((k) => typeof v[k] === "string" && v[k]);
  if (!key) return "";
  const val = v[key];
  return key === "path" ? val.split("/").pop() : val;
}

// clipToolText caps tool input/output previews: first TOOL_PREVIEW_LINES lines and first
// TOOL_PREVIEW_CHARS characters, ellipsis when clipped. Expand buttons show the full text.
function clipToolText(s) {
  let t = s;
  let clipped = false;
  if (t.length > TOOL_PREVIEW_CHARS) { t = t.slice(0, TOOL_PREVIEW_CHARS); clipped = true; }
  const lines = t.split("\n");
  if (lines.length > TOOL_PREVIEW_LINES) { t = lines.slice(0, TOOL_PREVIEW_LINES).join("\n"); clipped = true; }
  return clipped ? t + " …" : t;
}

// toolPre builds a tool-card <pre> showing the clipped preview; an expand button toggles
// to the full text when clipping occurred. Button nests inside the <pre> so the card keeps
// a single content node per input/output (matches .ev.tool pre styling scope).
function toolPre(full, cls) {
  const pre = document.createElement("pre");
  if (cls) pre.className = cls;
  const preview = clipToolText(full);
  pre.textContent = preview;
  if (preview === full) return pre;
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "expand-btn";
  btn.textContent = "展开完整内容";
  btn.addEventListener("click", () => {
    const expanded = btn.textContent === "收起";
    pre.textContent = expanded ? preview : full;
    btn.textContent = expanded ? "展开完整内容" : "收起";
    pre.appendChild(btn);
  });
  pre.appendChild(btn);
  return pre;
}

export { STEP_ICONS, getIcon, addCopyButtons, makeCollapsible, toolArg, toolPre, clipToolText };
