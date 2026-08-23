"use strict";

// ---- shared state, auth, persistence helpers ----
// Session-bound state (session id, sending, tokens…) lives on the view objects in views.js,
// not here — several sessions are open at once.

const KEY = "miniagent.web.key";
const WD_KEY = "miniagent.web.workdir";
const MODEL_KEY = "miniagent.web.model";
const THEME_KEY = "miniagent.web.theme";
const COMPOSER_ADV_KEY = "miniagent.web.composerAdv";

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
export function saveComposerAdv(open) { localStorage.setItem(COMPOSER_ADV_KEY, open ? "1" : ""); }
export function loadComposerAdv() { return localStorage.getItem(COMPOSER_ADV_KEY) === "1"; }

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
  const wd = document.getElementById("session-workdir");
  wd.textContent = view?.workdir || "";
  wd.title = view?.workdir || "当前会话工作目录";
  const tok = document.getElementById("session-tokens");
  const t = view?.tokens || { in: 0, out: 0 };
  const total = t.in + t.out;
  const budget = view?.usage?.budget || budgetCache;
  let txt = total ? `in=${t.in} out=${t.out}` : "";
  if (budget > 0 && total > 0) txt += ` / ${budget}`;
  tok.textContent = txt;
  tok.title = "当前会话累计 token（来自 result 事件）";
}

// setVersion renders the server version into the login page and the footer status bar.
// The raw string is displayed as-is: it comes from miniagent.Version (git tag or hash),
// already carrying its own prefix when one is intended.
// Idempotent: safe to call on boot and on every login retry.
export function setVersion(v) {
  const text = v || "";
  for (const id of ["version-login", "status-version"]) {
    const el = document.getElementById(id);
    if (el) el.textContent = text;
  }
}

// setStatusModel fills the footer status bar with the current model.
export function setStatusModel(m) {
  const el = document.getElementById("status-model");
  if (el) el.textContent = m || "";
}

// ---- status bar metrics ----

export function fmtDuration(ms) {
  if (!ms || ms <= 0) return "";
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  return `${Math.floor(s / 60)}m${String(s % 60).padStart(2, "0")}s`;
}

export function fmtTokens(n) {
  if (!n) return "";
  if (n >= 1e6) return (n / 1e6).toFixed(1) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(1) + "k";
  return String(n);
}

// updateMetrics renders the per-session counters into the status bar. All fields optional;
// an empty stats object clears the metrics span (version/model remain as the idle display).
export function updateMetrics(stats) {
  const el = document.getElementById("status-metrics");
  if (!el) return;
  const parts = [];
  if (stats.rounds) parts.push(`${stats.rounds}轮`);
  if (stats.steps) parts.push(`${stats.steps}步`);
  if (stats.llmMs) parts.push(`LLM ${fmtDuration(stats.llmMs)}`);
  if (stats.toolMs) parts.push(`工具 ${fmtDuration(stats.toolMs)}`);
  if (stats.inputTotal) parts.push(`输入 ${fmtTokens(stats.inputTotal)} tok`);
  el.textContent = parts.join("  ·  ");
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
