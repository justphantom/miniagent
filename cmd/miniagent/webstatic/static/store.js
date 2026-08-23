"use strict";

// ---- shared state, auth, persistence helpers ----
// Session-bound state (session id, sending, tokens…) lives on the view objects in views.js,
// not here — several sessions are open at once.

const KEY = "miniagent.web.key";
const WD_KEY = "miniagent.web.workdir";
const MODEL_KEY = "miniagent.web.model";
const THEME_KEY = "miniagent.web.theme";

export const state = {
  key: localStorage.getItem(KEY) || "",
};

export function setKey(k) { state.key = k; if (k) localStorage.setItem(KEY, k); else localStorage.removeItem(KEY); }
export function saveWorkdir(wd) { localStorage.setItem(WD_KEY, wd); }
export function loadWorkdir() { return localStorage.getItem(WD_KEY) || ""; }
export function saveModel(m) { localStorage.setItem(MODEL_KEY, m); }
export function loadModel() { return localStorage.getItem(MODEL_KEY) || ""; }
export function saveTheme(t) { localStorage.setItem(THEME_KEY, t); }
export function loadTheme() { return localStorage.getItem(THEME_KEY) || ""; }

export function authHeaders() { return { "x-api-key": state.key }; }

export async function api(path, opts = {}) {
  const r = await fetch(path, { ...opts, headers: { ...authHeaders(), ...(opts.headers || {}) } });
  if (r.status === 401) { setKey(""); location.reload(); }
  if (!r.ok) { let msg = r.statusText; try { msg = (await r.json()).error || msg; } catch { /* non-JSON error body */ } throw new Error(msg); }
  return r;
}

export function fmtTime(ms) {
  if (!ms) return "";
  const d = new Date(ms), p = (n) => String(n).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

// showSessionID renders the header for a view: session id plus the view's accumulated tokens.
export function showSessionID(view) {
  const el = document.getElementById("session-id");
  const id = view?.id || "";
  el.textContent = id ? `会话 ${id}` : "";
  el.title = id || "当前会话 ID";
  const tok = document.getElementById("session-tokens");
  const t = view?.tokens || { in: 0, out: 0 };
  tok.textContent = t.in || t.out ? `in=${t.in} out=${t.out}` : "";
  tok.title = "当前会话累计 token（来自 result 事件）";
}

// setVersion renders the server version (from /api/whoami) into both the login page and the header badge.
// Idempotent: safe to call on boot and on every login retry.
export function setVersion(v) {
  const text = v ? `v${v}` : "";
  for (const id of ["version-login", "version-badge"]) {
    const el = document.getElementById(id);
    if (el) el.textContent = text;
  }
}

// setModelBadge fills the header's current-model badge: from the model dropdown selection
// or the result event's model field (covers config-default turns where no dropdown item matched).
export function setModelBadge(m) {
  const el = document.getElementById("model-badge");
  if (el) el.textContent = m || "";
}

let budgetCache = 0;
export function getBudget() { return budgetCache; }
export async function refreshBudget() {
  try {
    const r = await api("/api/config");
    const cfg = await r.json();
    budgetCache = cfg?.config?.run?.max_tokens_total || 0;
  } catch { /* ignore */ }
}
