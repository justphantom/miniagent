"use strict";

// dirpicker.js — Directory picker modal: browse /api/tree and pick a workdir.

import { authHeaders } from "./store.js";

const RECENT_KEY = "miniagent.web.recent_dirs";
const MAX_RECENT = 10;
let curPath = "";
let onPickCb = null;
let pickerEl = null;
let fetchSeq = 0; // F11: guards against out-of-order responses when clicking dirs fast

function loadRecent() {
  try { return JSON.parse(localStorage.getItem(RECENT_KEY) || "[]"); }
  catch { return []; }
}

function saveRecent(p) {
  let list = loadRecent().filter(v => v !== p);
  list.unshift(p);
  if (list.length > MAX_RECENT) list = list.slice(0, MAX_RECENT);
  localStorage.setItem(RECENT_KEY, JSON.stringify(list));
}

function buildDOM() {
  if (pickerEl) return;
  // F3: reuse the static empty shell in index.html instead of creating a duplicate id
  pickerEl = document.getElementById("dirpicker");
  if (!pickerEl) {
    pickerEl = document.createElement("div");
    pickerEl.id = "dirpicker";
    pickerEl.className = "modal";
    document.body.appendChild(pickerEl);
  }
  pickerEl.hidden = true;
  pickerEl.innerHTML = `
    <div class="modal-panel" role="dialog" aria-label="选择工作目录">
      <div class="modal-head">
        <span class="modal-title">选择工作目录</span>
        <button class="ghost" id="dp-close" aria-label="关闭">✕</button>
      </div>
      <div class="modal-body">
        <div class="dp-recent" id="dp-recent"></div>
        <div class="dp-path-bar">
          <button class="ghost" id="dp-up" aria-label="上级目录">↰</button>
          <span class="dp-path" id="dp-path"></span>
        </div>
        <div class="dp-tree" id="dp-tree"></div>
      </div>
      <div class="modal-foot">
        <button class="ghost" id="dp-cancel">取消</button>
        <button id="dp-choose">选择此目录</button>
      </div>
    </div>`;
  document.body.appendChild(pickerEl);
  document.getElementById("dp-close").addEventListener("click", closePicker);
  document.getElementById("dp-cancel").addEventListener("click", closePicker);
  document.getElementById("dp-choose").addEventListener("click", chooseDir);
  document.getElementById("dp-up").addEventListener("click", goUp);
  pickerEl.addEventListener("click", (e) => { if (e.target === pickerEl) closePicker(); });
  pickerEl.addEventListener("keydown", (e) => { if (e.key === "Escape") closePicker(); });
}

async function fetchDirs(path) {
  const seq = ++fetchSeq;
  const treeEl = document.getElementById("dp-tree");
  treeEl.textContent = "加载中…";
  try {
    const r = await fetch(`/api/tree?path=${encodeURIComponent(path)}`, { headers: authHeaders() });
    if (!r.ok) {
      const j = await r.json().catch(() => ({}));
      if (seq !== fetchSeq) return;
      treeEl.textContent = j.error || `加载失败 (${r.status})`;
      return;
    }
    const data = await r.json();
    if (seq !== fetchSeq) return;
    curPath = data.path;
    document.getElementById("dp-path").textContent = curPath;
    treeEl.textContent = "";
    if (!data.dirs || data.dirs.length === 0) {
      treeEl.textContent = "无子目录";
      return;
    }
    for (const d of data.dirs) {
      const item = document.createElement("div");
      item.className = "dp-tree-item";
      item.textContent = d.name + "/";
      item.addEventListener("click", () => fetchDirs(d.path));
      treeEl.appendChild(item);
    }
  } catch (e) {
    if (seq !== fetchSeq) return;
    treeEl.textContent = "网络错误: " + e.message;
  }
}

function renderRecent() {
  const box = document.getElementById("dp-recent");
  box.textContent = "";
  const list = loadRecent();
  for (const p of list) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "dp-recent-item";
    btn.textContent = p;
    btn.addEventListener("click", () => fetchDirs(p));
    box.appendChild(btn);
  }
}

function goUp() {
  if (!curPath || curPath === "/") return;
  const parent = curPath.replace(/\/[^/]+\/?$/, "") || "/";
  fetchDirs(parent);
}

function chooseDir() {
  saveRecent(curPath);
  if (onPickCb) onPickCb(curPath);
  closePicker();
}

function closePicker() {
  if (pickerEl) pickerEl.hidden = true;
}

// openPicker opens the directory picker starting at startPath.
function openPicker(startPath) {
  buildDOM();
  renderRecent();
  pickerEl.hidden = false;
  document.getElementById("dp-close").focus();
  fetchDirs(startPath || "/");
}

// attachDirPicker sets up the workdir browse button and exposes openPicker.
export function attachDirPicker(opts) {
  onPickCb = opts.onPick;
  const browseBtn = document.getElementById("workdir-browse");
  if (browseBtn) {
    browseBtn.addEventListener("click", () => {
      const wd = document.getElementById("workdir")?.value?.trim() || "/";
      openPicker(wd);
    });
  }
}
