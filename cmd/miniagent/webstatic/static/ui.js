"use strict";

// ui.js — composer/status-bar UI coordination shared across modules: header+composer
// reflection, inline hint/confirm dialogs, wait indicator, shared auto-scroll observer.

import { showSessionHeader } from "./store.js";
import { activeView, refreshMetrics, eventsViewport, jumpToBottom } from "./views.js";

export const $ = (id) => document.getElementById(id);

// ---- header / composer reflect the ACTIVE view ----

export function updateHeader() {
  const v = activeView();
  document.title = v?.id ? `miniagent · ${v.id}` : "miniagent";
  showSessionHeader(v);
  refreshMetrics(v);
  updateComposer();
}

export function updateComposer() {
  const v = activeView();
  const busy = !!v?.running;
  $("send").textContent = busy ? "■" : "➤";
  $("send").setAttribute("aria-label", busy ? "停止" : "发送");
  $("send").setAttribute("title", busy ? "停止当前轮次" : "发送（运行中点击停止）");
  $("send").classList.toggle("danger", busy);
  $("prompt").disabled = false;
  $("workdir").disabled = false;
  $("model").disabled = false;
}

// Inline validation hint (replaces alert()): transient, next to the composer.
export function inlineHint(msg) {
  const el = $("wait");
  el.hidden = false;
  el.textContent = msg;
  el.classList.add("msg-error");
  setTimeout(() => { el.classList.remove("msg-error"); el.hidden = true; el.textContent = ""; }, 2500);
}

// Inline confirm dialog (replaces native confirm()): resolves true on confirm, false otherwise.
export function confirmInline(msg, okText) {
  const overlay = document.createElement("div");
  overlay.setAttribute("role", "dialog");
  overlay.setAttribute("aria-modal", "true");
  overlay.className = "confirm-overlay";
  const box = document.createElement("div");
  box.className = "confirm-box";
  const p = document.createElement("p");
  p.textContent = msg;
  const btnOk = document.createElement("button");
  btnOk.textContent = okText;
  btnOk.className = "confirm-ok";
  const btnCancel = document.createElement("button");
  btnCancel.textContent = "取消";
  btnCancel.className = "confirm-cancel";
  const row = document.createElement("div");
  row.className = "confirm-row";
  row.append(btnCancel, btnOk);
  box.append(p, row);
  overlay.append(box);
  document.body.append(overlay);
  btnCancel.focus();
  const focusables = [btnCancel, btnOk];
  overlay.addEventListener("keydown", (e) => {
    if (e.key !== "Tab") return;
    const last = focusables[focusables.length - 1], first = focusables[0];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  });
  return new Promise(resolve => {
    const close = (val) => { overlay.remove(); resolve(val); };
    btnOk.addEventListener("click", () => close(true));
    btnCancel.addEventListener("click", () => close(false));
    overlay.addEventListener("click", (e) => { if (e.target === overlay) close(false); });
    overlay.addEventListener("keydown", (e) => {
      if (e.key === "Escape") close(false);
      else if (e.key === "Enter" && document.activeElement === btnOk) close(true);
    });
  });
}

// Wait indicator for non-streaming configs: between send and the first event the UI would
// otherwise look dead (no deltas arrive until the terminal result).
let waitTimer = 0;
export function startWait() {
  const el = $("wait");
  let dots = 0;
  el.hidden = false;
  el.classList.remove("msg-error");
  waitTimer = setInterval(() => { dots = (dots + 1) % 4; el.textContent = `等待响应${".".repeat(dots)}`; }, 400);
}
export function stopWait() {
  if (!waitTimer) return;
  clearInterval(waitTimer);
  waitTimer = 0;
  $("wait").hidden = true;
}

// ---- shared auto-scroll observer ----
// Hidden views mutating do not change scrollHeight (display:none), so one observer keyed to
// the active view's stickBottom covers all streams without cross-view jumps. Shared by the
// live turn stream (send.js) and the replay stream (sessions.js).
const scrollMo = new MutationObserver(() => {
  const v = activeView();
  if (v?.stickBottom) jumpToBottom();
});
export function observeScroll() {
  scrollMo.observe(eventsViewport(), { childList: true, subtree: true, characterData: true });
}
export function disconnectScroll() { scrollMo.disconnect(); }
