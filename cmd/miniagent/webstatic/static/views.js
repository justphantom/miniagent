"use strict";

// views.js — the multi-session view registry. Each session (or a fresh "__new_N" draft) owns
// a view: a detached-capable DOM subtree of #events plus its streaming state. Switching views
// swaps visibility instead of aborting anything — turns keep running server-side (D1) and
// their streams keep rendering into their (possibly hidden) view.

import { updateMetrics } from "./store.js";

const MAX_VIEWS = 8; // detached-DOM residency bound; idle views beyond this are dropped

const views = new Map(); // key → view (key = session id, or "__new_N" for drafts)
let active = null;
let seq = 0;

export function eventsViewport() { return document.getElementById("events"); }

// createView builds and registers a view. id "" = a new-chat draft, upgraded to the real id
// by rekey() when the session event arrives.
export function createView(id = "") {
  evictIdle();
  const key = id || `__new_${++seq}`;
  const dom = document.createElement("div");
  dom.className = "view";
  const view = {
    key,
    id, // "" until the first turn's session event
    dom,
    gen: 0, // M8 per view: streams capture it and drop events superseded by a newer stream
    sending: false, // a local POST /api/turn is in flight for this view
    running: false, // a turn is in flight anywhere (this browser or another)
    abort: null,
    liveDetach: null,
    lastPrompt: "",
    tokens: { in: 0, out: 0 },
    turnStartTs: 0,
    curText: null,
    curReasoning: null,
    toolNodes: new Map(),
    stickBottom: true,
    workdir: "",
    lastUsed: Date.now(),
    usage: { budget: 0, steps: [] },
    trajectory: { order: [], steps: new Map() },
    curStep: 0,
    metrics: { rounds: 0, steps: 0, llmMs: 0, toolMs: 0 }, // status-bar counters, rebuilt on replay
  };
  dom.dataset.viewKey = key;
  views.set(key, view);
  eventsViewport().appendChild(dom);
  if (active) dom.style.display = "none";
  return view;
}

// rekey upgrades a draft view to its server-assigned session id.
export function rekey(view, newID) {
  if (!newID || newID === view.key || views.has(newID)) return;
  views.delete(view.key);
  view.key = newID;
  view.id = newID;
  views.set(newID, view);
}

export function byID(id) { return id ? views.get(id) : null; }

export function activate(view) {
  if (!view) return;
  if (active && active !== view) active.dom.style.display = "none";
  const wasHidden = active !== view;
  active = view;
  view.dom.style.display = "";
  view.lastUsed = Date.now();
  view.stickBottom = true; // entering a view means "take me to the latest"
  if (wasHidden) jumpToBottom();
}

export function activeView() { return active; }

// refreshMetrics pushes a view's counters into the status bar (empty for drafts).
export function refreshMetrics(view) {
  const m = view?.metrics;
  updateMetrics(m ? { rounds: m.rounds, steps: m.steps, llmMs: m.llmMs, toolMs: m.toolMs, inputTotal: view.tokens.in } : {});
}

export function dropView(view) {
  if (!view) return;
  view.abort?.abort();
  view.liveDetach?.();
  views.delete(view.key);
  view.dom.remove();
  if (active === view) active = null;
}

export function jumpToBottom() {
  const el = eventsViewport();
  el.scrollTop = el.scrollHeight;
}

// evictIdle drops the least-recently-used idle views when residency exceeds MAX_VIEWS.
// Active and running views are never evicted (a running turn's stream needs its DOM).
function evictIdle() {
  if (views.size < MAX_VIEWS) return;
  const idle = [...views.values()]
    .filter(v => v !== active && !v.sending && !v.running)
    .sort((a, b) => a.lastUsed - b.lastUsed);
  for (const v of idle) {
    if (views.size < MAX_VIEWS) break;
    dropView(v);
  }
}
