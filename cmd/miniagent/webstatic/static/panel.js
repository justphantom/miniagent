"use strict";

// panel.js — slide-in session panel: open/close/toggle, outside-click and Esc dismissal.
// The panel lives inside <main> (absolute overlay), so it never overlaps status bar/input.

const $ = (id) => document.getElementById(id);

export function isPanelOpen() { return !$("session-panel").hidden; }

export function openPanel() {
  $("session-panel").hidden = false;
  $("icon-sessions").classList.add("active");
}

export function closePanel() {
  $("session-panel").hidden = true;
  $("icon-sessions").classList.remove("active");
}

export function togglePanel() { isPanelOpen() ? closePanel() : openPanel(); }

// initPanel wires close interactions: ✕ button, Esc, and clicks on the events area
// (outside the panel). Called once from app.js.
export function initPanel() {
  $("panel-close").addEventListener("click", closePanel);
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && isPanelOpen()) closePanel();
  });
  $("events").addEventListener("click", () => { if (isPanelOpen()) closePanel(); });
}

// filterSessions narrows the list by the search input (client-side substring match);
// tree groups with no visible items collapse too.
export function filterSessions(q) {
  const needle = (q || "").trim().toLowerCase();
  for (const item of document.querySelectorAll("#session-list .sess-item")) {
    item.style.display = !needle || item.textContent.toLowerCase().includes(needle) ? "" : "none";
  }
  for (const g of document.querySelectorAll("#session-list .tree-group")) {
    const anyVisible = [...g.querySelectorAll(".sess-item")].some((i) => i.style.display !== "none");
    g.style.display = anyVisible ? "" : "none";
  }
}
