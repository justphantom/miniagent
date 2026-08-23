"use strict";

// trajectory.js — Tool trajectory panel: step-grouped tool call cards with scroll-to-step.

import { fmtTime } from "./store.js";

let panelEl = null;
let bodyEl = null;

// ensurePanel lazily creates the trajectory panel DOM inside #layout.
function ensurePanel() {
  if (panelEl) return;
  panelEl = document.createElement("div");
  panelEl.id = "trajectory-panel";
  panelEl.className = "trajectory-panel";
  panelEl.hidden = true;
  const head = document.createElement("div");
  head.className = "trajectory-head";
  const title = document.createElement("span");
  title.textContent = "轨迹";
  const close = document.createElement("button");
  close.className = "ghost";
  close.id = "tj-close";
  close.setAttribute("aria-label", "关闭轨迹");
  close.textContent = "✕";
  close.addEventListener("click", () => toggleTrajectory());
  head.append(title, close);
  bodyEl = document.createElement("div");
  bodyEl.id = "tj-body";
  bodyEl.className = "trajectory-body";
  panelEl.append(head, bodyEl);
  const layout = document.getElementById("layout");
  if (layout) layout.appendChild(panelEl);
}

// toggleTrajectory shows/hides the trajectory panel and toggles the layout class.
export function toggleTrajectory() {
  ensurePanel();
  const wasHidden = panelEl.hidden;
  panelEl.hidden = !wasHidden;
  document.getElementById("layout")?.classList.toggle("trajectory-open", wasHidden);
  if (wasHidden) refreshPanel();
}

// refreshPanel rebuilds the panel body from the active view's trajectory data,
// preserving which step cards were expanded.
export function refreshPanel() {
  ensurePanel();
  const view = _activeViewGetter ? _activeViewGetter() : null;
  if (!view || !view.trajectory) { bodyEl.textContent = ""; return; }
  const openSteps = new Set(
    [...bodyEl.querySelectorAll("details.trajectory-step[open]")].map(d => d.dataset.step)
  );
  renderBody(view);
  for (const d of bodyEl.querySelectorAll("details.trajectory-step")) {
    if (openSteps.has(d.dataset.step)) d.open = true;
  }
}

// renderBody fills the trajectory panel with step cards.
function renderBody(view) {
  bodyEl.textContent = "";
  const { order, steps } = view.trajectory;
  for (const stepNo of order) {
    const entry = steps.get(stepNo);
    if (!entry) continue;
    bodyEl.appendChild(renderStepCard(view, entry));
  }
}

// renderStepCard builds a <details> card for one step.
function renderStepCard(view, entry) {
  const det = document.createElement("details");
  det.className = "trajectory-step";
  det.dataset.step = entry.step;
  const head = document.createElement("summary");
  head.className = "trajectory-step-head";
  const stepNo = document.createElement("span");
  stepNo.className = "tj-step-no";
  stepNo.textContent = `Step ${entry.step}`;
  const meta = document.createElement("span");
  meta.className = "tj-step-meta";
  const usage = entry.in || entry.out ? `in=${entry.in} out=${entry.out}` : "—";
  meta.textContent = `${usage} · ${entry.tools.length} 工具`;
  head.append(stepNo, meta);
  head.addEventListener("click", () => scrollToStep(view, entry.step));
  det.appendChild(head);
  const toolsDiv = document.createElement("div");
  toolsDiv.className = "trajectory-tools";
  for (const t of entry.tools) {
    const toolDet = document.createElement("details");
    toolDet.className = "trajectory-tool";
    const toolSum = document.createElement("summary");
    toolSum.textContent = t.name;
    const time = document.createElement("span");
    time.className = "time";
    time.textContent = fmtTime(t.ts);
    toolSum.appendChild(time);
    const preIn = document.createElement("pre");
    preIn.className = "tj-input";
    preIn.textContent = t.input || "";
    toolDet.append(toolSum, preIn);
    if (t.output) {
      const preOut = document.createElement("pre");
      preOut.className = "out" + (t.isError ? " err" : "");
      preOut.textContent = t.output;
      toolDet.appendChild(preOut);
    }
    toolsDiv.appendChild(toolDet);
  }
  det.appendChild(toolsDiv);
  return det;
}

// scrollToStep finds the first DOM node with data-step in the events viewport and scrolls to it.
function scrollToStep(view, step) {
  const node = view.dom.querySelector(`[data-step="${step}"]`);
  if (node) {
    node.scrollIntoView({ behavior: "smooth", block: "start" });
    node.classList.add("pulse-highlight");
    setTimeout(() => node.classList.remove("pulse-highlight"), 2000);
  }
}

// _activeViewGetter is set by app.js to inject the activeView function (avoids circular deps).
let _activeViewGetter = null;
export function setActiveViewGetter(fn) { _activeViewGetter = fn; }
